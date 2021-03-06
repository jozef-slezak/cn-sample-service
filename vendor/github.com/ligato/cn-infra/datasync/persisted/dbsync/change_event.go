// Copyright (c) 2017 Cisco and/or its affiliates.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at:
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package dbsync

import (
	"encoding/json"
	"github.com/golang/protobuf/proto"
	"github.com/ligato/cn-infra/datasync"
	"github.com/ligato/cn-infra/datasync/syncbase"
	"github.com/ligato/cn-infra/db"
	"github.com/ligato/cn-infra/db/keyval"
)

// NewChangeWatchResp is a constructor
func NewChangeWatchResp(delegate keyval.BytesWatchResp, prevVal datasync.LazyValue) *ChangeWatchResp {
	return &ChangeWatchResp{delegate, prevVal, &syncbase.DoneChannel{DoneChan: nil}}
}

// ChangeWatchResp is a simple structure that adapts the WatchRest to the
// Basically only the callback is the value add.
type ChangeWatchResp struct {
	delegate keyval.BytesWatchResp
	prev     datasync.LazyValue
	*syncbase.DoneChannel
}

// GetChangeType - see the comment in implemented interface datasync.ChangeEvent
func (ev *ChangeWatchResp) GetChangeType() db.PutDel {
	return ev.delegate.GetChangeType()
}

// GetKey returns the key associated with the change
func (ev *ChangeWatchResp) GetKey() string {
	return ev.delegate.GetKey()
}

// GetValue delegates to WatchResp. For description of parameter and output values see the comment
// in implemented interface datasync.ChangeEvent
func (ev *ChangeWatchResp) GetValue(val proto.Message) (err error) {
	if ev.delegate.GetChangeType() != db.Delete {
		return json.Unmarshal(ev.delegate.GetValue(), val)
	}

	return nil
}

// GetPrevValue delegates to WatchResp. For description of parameter and output values see the comment
// in implemented interface datasync.ChangeEvent
func (ev *ChangeWatchResp) GetPrevValue(prevVal proto.Message) (exists bool, err error) {
	if ev.prev != nil {
		return true, ev.prev.GetValue(prevVal)
	}
	return false, nil
}

// GetRevision returns revision associated with the change.
func (ev *ChangeWatchResp) GetRevision() (rev int64) {
	return ev.delegate.GetRevision()
}
