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

package syncbase

import (
	"github.com/ligato/cn-infra/datasync"
)

// NewResyncEventDB is a constructor
func NewResyncEventDB(its map[string] /*keyPrefix*/ datasync.KeyValIterator) *ResyncEventDB {
	return &ResyncEventDB{its, NewDoneChannel(make(chan error, 1))}
}

// NewResyncEvent is a constructor
func NewResyncEvent(m map[string] /*keyPrefix*/ []datasync.KeyVal) *ResyncEventDB {
	its := map[string] /*keyPrefix*/ datasync.KeyValIterator{}
	for keyPrefix, kvs := range m {
		its[keyPrefix] = NewKVIterator(kvs)
	}

	return &ResyncEventDB{its, NewDoneChannel(make(chan error, 1))}
}

// ResyncEventDB implements interface datasync.ResyncEvent (see comments in there)
type ResyncEventDB struct {
	its map[string] /*keyPrefix*/ datasync.KeyValIterator
	*DoneChannel
}

// GetValues ...
func (ev *ResyncEventDB) GetValues() map[string] /*keyPrefix*/ datasync.KeyValIterator {
	return ev.its
}
