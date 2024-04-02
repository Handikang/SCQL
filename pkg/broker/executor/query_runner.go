// Copyright 2023 Ant Group Co., Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package executor

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"golang.org/x/exp/slices"

	"github.com/secretflow/scql/pkg/broker/application"
	"github.com/secretflow/scql/pkg/broker/constant"
	"github.com/secretflow/scql/pkg/broker/services/common"
	"github.com/secretflow/scql/pkg/broker/storage"
	"github.com/secretflow/scql/pkg/executor"
	"github.com/secretflow/scql/pkg/infoschema"
	"github.com/secretflow/scql/pkg/interpreter"
	"github.com/secretflow/scql/pkg/interpreter/translator"
	"github.com/secretflow/scql/pkg/parser/model"
	"github.com/secretflow/scql/pkg/planner/core"
	pb "github.com/secretflow/scql/pkg/proto-gen/scql"
	"github.com/secretflow/scql/pkg/util/sliceutil"
)

type QueryRunner struct {
	session *application.Session
	// running info
	info   *translator.EnginesInfo
	is     infoschema.InfoSchema
	ccls   []*pb.SecurityConfig_ColumnControl
	tables []storage.TableMeta
	// need update info/is/ccls
	prepareAgain bool
}

func NewQueryRunner(session *application.Session) *QueryRunner {
	return &QueryRunner{
		session: session,
	}
}

func (r *QueryRunner) Clear() {
	r.info = nil
	r.is = nil
	r.ccls = nil
}

func (r *QueryRunner) GetEnginesInfo() *translator.EnginesInfo {
	return r.info
}

func (r *QueryRunner) SetPrepareAgain() {
	r.prepareAgain = true
}

func (r *QueryRunner) CreateChecksum() (map[string]application.Checksum, error) {
	s := r.session
	checksumMap := make(map[string]application.Checksum)
	for _, p := range s.ExecuteInfo.DataParties {
		tableSchemaCrypt := sha256.New()
		cclCrypt := sha256.New()
		tables := r.info.GetTablesByParty(p)
		sort.Slice(tables, func(i, j int) bool {
			return tables[i].String() < tables[j].String()
		})
		for _, t := range tables {
			tableSchemaCrypt.Write([]byte(t.String()))
			tableSchema, err := r.is.TableByName(model.NewCIStr(t.GetDbName()), model.NewCIStr(t.GetTableName()))
			if err != nil {
				return nil, err
			}
			columnInfos := tableSchema.Meta().Columns
			sort.Slice(columnInfos, func(i, j int) bool {
				return columnInfos[i].Name.String() < columnInfos[j].Name.String()
			})
			for _, col := range columnInfos {
				tableSchemaCrypt.Write([]byte(col.Name.String()))
				tableSchemaCrypt.Write([]byte(col.GetTypeDesc()))
			}
			var cclsForP []*pb.SecurityConfig_ColumnControl
			for _, ccl := range r.ccls {
				if ccl.TableName == t.GetTableName() && ccl.DatabaseName == t.GetDbName() {
					cclsForP = append(cclsForP, ccl)
				}
			}
			sort.Slice(cclsForP, func(i, j int) bool {
				return strings.Join([]string{cclsForP[i].TableName, cclsForP[i].ColumnName, cclsForP[i].PartyCode}, " ") <
					strings.Join([]string{cclsForP[j].TableName, cclsForP[j].ColumnName, cclsForP[j].PartyCode}, " ")
			})
			for _, ccl := range cclsForP {
				cclCrypt.Write([]byte(ccl.TableName))
				cclCrypt.Write([]byte(ccl.ColumnName))
				cclCrypt.Write([]byte(ccl.Visibility.String()))
			}
		}
		checksumMap[p] = application.Checksum{TableSchema: tableSchemaCrypt.Sum(nil), CCL: cclCrypt.Sum(nil)}
	}
	return checksumMap, nil
}

func (r *QueryRunner) ExchangeJobInfo(targetParty string) (*pb.ExchangeJobInfoResponse, error) {
	session := r.session
	executionInfo := session.ExecuteInfo
	selfCode := session.GetSelfPartyCode()
	req := &pb.ExchangeJobInfoRequest{
		ProjectId: executionInfo.ProjectID,
		JobId:     executionInfo.JobID,
		ClientId:  &pb.PartyId{Code: selfCode},
	}
	if slices.Contains(executionInfo.DataParties, targetParty) {
		serverChecksum, err := session.GetLocalChecksum(targetParty)
		if err != nil {
			return nil, fmt.Errorf("ExchangeJobInfo: %s", err)
		}
		req.ServerChecksum = &pb.Checksum{
			TableSchema: serverChecksum.TableSchema,
			Ccl:         serverChecksum.CCL,
		}
		logrus.Infof("exchange job info with party %s with request %s", targetParty, req.String())
	}

	url, err := session.App.PartyMgr.GetBrokerUrlByParty(targetParty)
	if err != nil {
		return nil, fmt.Errorf("ExchangeJobInfoStub: %v", err)
	}
	response := &pb.ExchangeJobInfoResponse{}
	// retry to make sure that peer broker has created session
	for i := 0; i < session.App.Conf.ExchangeJobInfoRetryTimes; i++ {
		err = executionInfo.InterStub.ExchangeJobInfo(url, req, response)
		if err != nil {
			return nil, fmt.Errorf("ExchangeJobInfoStub: %v", err)
		}
		if response.GetStatus().GetCode() == int32(pb.Code_SESSION_NOT_FOUND) {
			if i < session.App.Conf.ExchangeJobInfoRetryTimes-1 {
				time.Sleep(r.session.App.Conf.ExchangeJobInfoRetryInterval)
			}
			continue
		}
		if response.GetStatus().GetCode() == 0 {
			return response, nil
		}
		break
	}
	if response.Status == nil {
		return nil, fmt.Errorf("err response from party %s; response %+v", targetParty, response)
	}
	if response.Status.Code == int32(pb.Code_DATA_INCONSISTENCY) {
		return response, nil
	}
	return nil, fmt.Errorf("failed to exchange job info with %s return error %+v", targetParty, response.Status)
}

func (r *QueryRunner) prepareData(usedTableNames []string) (dataParties []string, workParties []string, err error) {
	session := r.session
	txn := session.App.MetaMgr.CreateMetaTransaction()
	defer func() {
		err = txn.Finish(err)
	}()
	var notFoundTables []string
	r.tables, notFoundTables, err = txn.GetTableMetasByTableNames(session.ExecuteInfo.ProjectID, usedTableNames)
	if err != nil {
		return
	}
	if len(notFoundTables) > 0 && !r.prepareAgain {
		var members []string
		members, err = txn.GetProjectMembers(session.ExecuteInfo.ProjectID)
		if err != nil {
			return
		}
		// finish old transaction
		txn.Finish(nil)
		err = common.AskProjectInfoFromParties(session.App, session.ExecuteInfo.ProjectID, notFoundTables, []string{}, sliceutil.Subtraction(members, []string{session.App.Conf.PartyCode}))
		if err != nil {
			logrus.Warningf("prepareData: get not found tables %+v err: %s", notFoundTables, err)
		}
		// use new transaction
		txn = session.App.MetaMgr.CreateMetaTransaction()
		// get tables schema again
		r.tables, notFoundTables, err = txn.GetTableMetasByTableNames(session.ExecuteInfo.ProjectID, usedTableNames)
		if err != nil {
			return
		}
		if len(notFoundTables) > 0 {
			return nil, nil, fmt.Errorf("prepareData: table %+v not found", notFoundTables)
		}
	}
	var parties []string
	party2Tables := make(map[string][]core.DbTable)
	tableToRefs := make(map[core.DbTable]core.DbTable)
	for _, t := range r.tables {
		parties = append(parties, t.Table.Owner)
		if _, exist := party2Tables[t.Table.Owner]; !exist {
			party2Tables[t.Table.Owner] = []core.DbTable{}
		}
		// currently use project id as db name
		dbTable := core.NewDbTable(t.Table.ProjectID, t.Table.TableName)
		party2Tables[t.Table.Owner] = append(party2Tables[t.Table.Owner], dbTable)
		var refDbTable core.DbTable
		refDbTable, err = core.NewDbTableFromString(t.Table.RefTable)
		if err != nil {
			return
		}
		var dbType core.DBType
		dbType, err = core.ParseDBType(t.Table.DBType)
		if err != nil {
			return
		}
		refDbTable.SetDBType(dbType)
		tableToRefs[dbTable] = refDbTable
	}
	// SliceDeDup sort parties and compact
	dataParties = sliceutil.SliceDeDup(parties)
	workParties = sliceutil.SliceDeDup(append(dataParties, session.ExecuteInfo.Issuer.Code))
	partyInfo, err := session.App.PartyMgr.GetPartyInfoByParties(workParties)
	if err != nil {
		return
	}
	r.info = translator.NewEnginesInfo(partyInfo, party2Tables)
	r.info.UpdateTableToRefs(tableToRefs)
	// get ccls
	columnPrivs, err := txn.ListColumnConstraints(session.ExecuteInfo.ProjectID, usedTableNames, workParties)
	for _, columnPriv := range columnPrivs {
		r.ccls = append(r.ccls, &pb.SecurityConfig_ColumnControl{
			PartyCode:    columnPriv.DestParty,
			Visibility:   pb.SecurityConfig_ColumnControl_Visibility(pb.SecurityConfig_ColumnControl_Visibility_value[strings.ToUpper(columnPriv.Priv)]),
			DatabaseName: columnPriv.ProjectID,
			TableName:    columnPriv.TableName,
			ColumnName:   columnPriv.ColumnName,
		})
	}
	return
}

func (r *QueryRunner) Prepare(usedTables []core.DbTable) (dataParties []string, workParties []string, err error) {
	// clear before preparing
	r.Clear()
	// get data from storage
	var usedTableNames []string
	for _, t := range usedTables {
		usedTableNames = append(usedTableNames, t.GetTableName())
	}
	dataParties, workParties, err = r.prepareData(usedTableNames)
	if err != nil {
		return
	}
	// create info schema
	r.is, err = r.CreateInfoSchema(r.tables)
	if err != nil {
		return
	}
	return
}

func (r *QueryRunner) CreateInfoSchema(tables []storage.TableMeta) (result infoschema.InfoSchema, err error) {
	s := r.session
	info := make(map[string][]*model.TableInfo)
	var tableInfos []*model.TableInfo
	for i, tbl := range tables {
		tblInfo := &model.TableInfo{
			ID:          int64(i),
			TableId:     fmt.Sprint(i),
			Name:        model.NewCIStr(tbl.Table.TableName),
			Columns:     []*model.ColumnInfo{},
			Indices:     []*model.IndexInfo{},
			ForeignKeys: []*model.FKInfo{},
			State:       model.StatePublic,
			PKIsHandle:  false,
		}
		// TODO: support view

		for i, col := range tbl.Columns {
			colTyp := strings.ToLower(col.DType)
			defaultVal, err := infoschema.TypeDefaultValue(colTyp)
			if err != nil {
				return nil, err
			}
			fieldTp, err := infoschema.TypeConversion(colTyp)
			if err != nil {
				return nil, err
			}
			colInfo := &model.ColumnInfo{
				ID:                 int64(i),
				Name:               model.NewCIStr(col.ColumnName),
				Offset:             i,
				OriginDefaultValue: defaultVal,
				DefaultValue:       defaultVal,
				DefaultValueBit:    []byte{},
				Dependences:        map[string]struct{}{},
				FieldType:          fieldTp,
				State:              model.StatePublic,
			}
			tblInfo.Columns = append(tblInfo.Columns, colInfo)
		}
		tableInfos = append(tableInfos, tblInfo)
	}
	info[s.ExecuteInfo.ProjectID] = tableInfos
	return infoschema.MockInfoSchema(info), nil
}

func (r *QueryRunner) buildCompileQueryRequest() *pb.CompileQueryRequest {
	s := r.session
	catalog := buildCatalog(r.tables)
	req := &pb.CompileQueryRequest{
		Query:  s.ExecuteInfo.Query,
		DbName: s.ExecuteInfo.ProjectID,
		Issuer: s.ExecuteInfo.Issuer,
		// In p2p, `IssuerAsParticipant` is always true.
		IssuerAsParticipant: true,
		SecurityConf: &pb.SecurityConfig{
			ColumnControlList: r.ccls,
		},
		Catalog:     catalog,
		CompileOpts: s.ExecuteInfo.CompileOpts,
	}
	return req
}

func buildCatalog(tables []storage.TableMeta) *pb.Catalog {
	catalog := &pb.Catalog{}
	for _, tbl := range tables {
		tblEntry := &pb.TableEntry{
			TableName: fmt.Sprintf("%s.%s", tbl.Table.ProjectID, tbl.Table.TableName),
			// TODO: support view
			IsView:   false,
			RefTable: tbl.Table.RefTable,
			DbType:   tbl.Table.DBType,
			Owner: &pb.PartyId{
				Code: tbl.Table.Owner,
			},
		}
		for _, col := range tbl.Columns {
			colEntry := &pb.TableEntry_Column{
				Name: col.ColumnName,
				Type: col.DType,
				// TODO: populate OrdinalPosition
				// OrdinalPosition: <pos>,
			}
			tblEntry.Columns = append(tblEntry.Columns, colEntry)
		}
		catalog.Tables = append(catalog.Tables, tblEntry)
	}
	return catalog
}

func (r *QueryRunner) CreateExecutor(plan *pb.CompiledPlan) (*executor.Executor, error) {
	// create SessionStartParams
	session := r.session
	conf := session.App.Conf
	startParams := &pb.SessionStartParams{
		PartyCode:     conf.PartyCode,
		SessionId:     session.ExecuteInfo.JobID,
		SpuRuntimeCfg: plan.GetSpuRuntimeConf(),
	}
	for i, p := range plan.Parties {
		endpoint, err := session.GetEndpoint(p.GetCode())
		if err != nil {
			return nil, err
		}
		pubKey, err := session.App.PartyMgr.GetPubKeyByParty(p.GetCode())
		if err != nil {
			return nil, err
		}
		startParams.Parties = append(startParams.Parties, &pb.SessionStartParams_Party{
			Code:      p.GetCode(),
			Name:      p.GetCode(),
			Rank:      int32(i),
			Host:      endpoint,
			PublicKey: pubKey,
		})
	}

	myGraph, exists := plan.GetSubGraphs()[conf.PartyCode]
	if !exists {
		return nil, fmt.Errorf("could not find my graph")
	}

	req := &pb.RunExecutionPlanRequest{
		SessionParams: startParams,
		Graph:         myGraph,
		Async:         false,
		DebugOpts:     session.ExecuteInfo.DebugOpts,
	}

	planReqs := map[string]*pb.RunExecutionPlanRequest{
		conf.PartyCode: req,
	}

	// create sync executor
	myPubKey, err := session.App.PartyMgr.GetPubKeyByParty(conf.PartyCode)
	if err != nil {
		return nil, err
	}

	myself := &translator.Participant{
		PartyCode: conf.PartyCode,
		Endpoints: []string{session.Engine.GetEndpointForSelf()},
		PubKey:    myPubKey,
	}

	partyInfo := translator.NewPartyInfo([]*translator.Participant{myself})

	engineStub := executor.NewEngineStub(
		session.ExecuteInfo.JobID,
		conf.IntraServer.Protocol,
		session.CallBackHost,
		constant.EngineCallbackPath,
		session.ExecuteInfo.EngineClient,
		conf.Engine.Protocol,
		conf.Engine.ContentType,
		partyInfo,
	)

	// p2p: party code who is not issuer doesn't have output tensors
	var outputNames []string
	if session.IsIssuer() {
		for _, col := range plan.GetSchema().GetColumns() {
			outputNames = append(outputNames, col.GetName())
		}
	}

	return executor.NewExecutor(planReqs, outputNames, engineStub, r.session.ExecuteInfo.JobID, translator.NewPartyInfo([]*translator.Participant{myself}))
}

// checkChecksum checks data consistency with other parties via comparing checksum
func (r *QueryRunner) checkChecksum() error {
	executionInfo := r.session.ExecuteInfo
	for _, p := range executionInfo.DataParties {
		if p == r.session.GetSelfPartyCode() {
			continue
		}
		compareResult, err := executionInfo.Checksums.CompareChecksumFor(p)
		if err != nil {
			return err
		}
		if compareResult != pb.ChecksumCompareResult_EQUAL {
			return fmt.Errorf("checksum not equal with party %s", p)
		}
	}
	return nil
}

func (r *QueryRunner) Execute(usedTables []core.DbTable) error {
	s := r.session
	if r.prepareAgain {
		logrus.Infof("ask info has been triggered, get data from storage again")
		_, _, err := r.Prepare(usedTables)
		if err != nil {
			return err
		}
		localChecksums, err := r.CreateChecksum()
		if err != nil {
			return err
		}
		for code, checksum := range localChecksums {
			s.SaveLocalChecksum(code, checksum)
		}
		// check checksum again
		if err := r.checkChecksum(); err != nil {
			return err
		}
	}

	compileReq := r.buildCompileQueryRequest()
	intrpr := interpreter.NewInterpreter()
	compiledPlan, err := intrpr.Compile(context.Background(), compileReq)
	if err != nil {
		return fmt.Errorf("failed to compile query to plan: %w", err)
	}

	logrus.Infof("Execution Plan:\n%s\n", compiledPlan.GetExplain().GetExeGraphDot())

	executor, err := r.CreateExecutor(compiledPlan)
	if err != nil {
		return err
	}
	s.OutputNames = executor.OutputNames
	s.Warning = compiledPlan.Warning
	if s.IsIssuer() {
		// we must persist session info before executing to avoid engine reporting failure
		// persist session info need to contain more infos: Warning/OutputNames...
		err = s.App.PersistSessionInfo(s)
		if err != nil {
			return fmt.Errorf("runQuery persist session info err: %v", err)
		}
	}
	// TODO: sync err to issuer
	ret, err := executor.RunExecutionPlan(s.Ctx, s.AsyncMode)
	if err != nil {
		return err
	}
	if ret.GetStatus().GetCode() != 0 {
		return fmt.Errorf("status: %s", ret)
	}

	// store result to session when engines run in sync mode
	if !s.AsyncMode {
		result := &pb.QueryResponse{
			Status:       ret.Status,
			OutColumns:   ret.GetOutColumns(),
			AffectedRows: ret.GetAffectedRows(),
			CostTimeS:    time.Since(s.CreatedAt).Seconds(),
		}
		if compiledPlan.Warning.MayAffectedByGroupThreshold {
			reason := "for safety, we filter the results for groups which contain less than 4 items."
			logrus.Infof("%v", reason)
			result.Warnings = append(result.Warnings, &pb.SQLWarning{Reason: reason})
		}

		s.SetResultSafely(result)
	} // when engines run in async mode, result will be set in callback handler.

	return nil
}

// get checksum from parties except self and issuer
func (r *QueryRunner) GetChecksumFromOtherParties(issuerParty string) error {
	session := r.session
	for _, p := range session.ExecuteInfo.DataParties {
		if p == issuerParty || p == r.session.GetSelfPartyCode() {
			continue
		}
		response, err := r.ExchangeJobInfo(p)
		if err != nil {
			return err
		}
		err = session.SaveRemoteChecksum(p, response.ExpectedServerChecksum)
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *QueryRunner) DryRun(usedTables []core.DbTable) error {
	// 1. check data consistency
	if err := r.checkChecksum(); err != nil {
		return err
	}
	// 2. try compile query
	compileReq := r.buildCompileQueryRequest()
	intrpr := interpreter.NewInterpreter()
	_, err := intrpr.Compile(context.TODO(), compileReq)
	if err != nil {
		return fmt.Errorf("failed to compile query: %w", err)
	}
	return nil
}
