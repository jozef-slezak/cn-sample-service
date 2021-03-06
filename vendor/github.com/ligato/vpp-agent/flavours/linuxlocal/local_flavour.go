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

package linuxlocal

import (
	"github.com/ligato/cn-infra/core"
	"github.com/ligato/cn-infra/datasync/resync"
	"github.com/ligato/cn-infra/httpmux"
	"github.com/ligato/cn-infra/logging/logmanager"
	"github.com/ligato/cn-infra/logging/logrus"
	"github.com/ligato/cn-infra/servicelabel"
	"github.com/ligato/cn-infra/statuscheck"
	"github.com/ligato/vpp-agent/clientv1/linux/localclient"
	"github.com/ligato/vpp-agent/plugins/defaultplugins"
	"github.com/ligato/vpp-agent/plugins/govppmux"
)

// Flavour glues together multiple plugins to mange VPP and linux interfaces configuration using local client.
type Flavour struct {
	injected         bool
	Logrus           logrus.Plugin
	LinuxLocalClient localclient.Plugin
	HTTP             httpmux.Plugin
	LogManager       logmanager.Plugin
	ServiceLabel     servicelabel.Plugin
	StatusCheck      statuscheck.Plugin
	Resync           resync.Plugin
	GoVPP            govppmux.GOVPPPlugin
	VPP              defaultplugins.Plugin
}

// Inject sets object references
func (f *Flavour) Inject() error {
	if f.injected {
		return nil
	}
	f.injected = true
	f.HTTP.LogFactory = &f.Logrus
	f.LogManager.ManagedLoggers = &f.Logrus
	f.LogManager.HTTP = &f.HTTP
	f.StatusCheck.HTTP = &f.HTTP
	f.GoVPP.StatusCheck = &f.StatusCheck
	f.GoVPP.LogFactory = &f.Logrus
	f.VPP.ServiceLabel = &f.ServiceLabel

	return nil
}

// Plugins combines Generic Plugins and Standard VPP Plugins + (their ETCD Connector/Adapter with RESYNC)
func (f *Flavour) Plugins() []*core.NamedPlugin {
	f.Inject()
	return core.ListPluginsInFlavor(f)
}
