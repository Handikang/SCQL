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

package application

import (
	"os"
	"time"

	"github.com/sirupsen/logrus"
)

// NOTE: StorageGc will continue GC until program exits
func (app *App) StorageGc() {
	// check locking info exists in table
	err := app.MetaMgr.InitGcLockIfNecessary()
	if err != nil {
		logrus.Errorf("failed to check gc lock %v", err)
		return
	}

	// Question: should we reuse the SessionCheckInterval ? it maybe too often
	ticker := time.NewTicker(app.Conf.SessionCheckInterval)
	owner := app.Conf.PartyCode
	if host := os.Getenv("HOSTNAME"); host != "" {
		owner = host
	} else {
		logrus.Warnf("cannot find HOSTNAME env, using party code as owner")
	}
	for {
		<-ticker.C

		// hold lock or continue for retry
		err := app.MetaMgr.HoldGcLock(owner, app.Conf.SessionCheckInterval)
		if err != nil {
			continue
		}

		// scan table to get all expired ids
		err = app.MetaMgr.ClearExpiredResults(app.Conf.SessionExpireTime)
		if err != nil {
			logrus.Warnf("GC err: %s", err.Error())
		}

	}
}

// NOTE: SessionGc will continue GC until program exits
func (app *App) SessionGc() {
	ticker := time.NewTicker(app.Conf.SessionCheckInterval)
	for {
		<-ticker.C

		var ids []string
		items := app.Sessions.Items()
		for k := range items {
			ids = append(ids, k)
		}

		canceledIds, err := app.MetaMgr.CheckIdCanceled(ids)
		if err != nil {
			logrus.Errorf("check canceled session failed: %v", err)
			continue
		}
		for _, id := range canceledIds {
			app.DeleteSession(id)
		}
	}

}
