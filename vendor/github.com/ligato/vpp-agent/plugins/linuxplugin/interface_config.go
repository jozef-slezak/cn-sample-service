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

//go:generate protoc --proto_path=model --gogo_out=model model/interfaces/interfaces.proto

package linuxplugin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/fsouza/go-dockerclient"

	log "github.com/ligato/cn-infra/logging/logrus"
	"github.com/ligato/vpp-agent/idxvpp"
	"github.com/ligato/vpp-agent/plugins/linuxplugin/linuxcalls"
	intf "github.com/ligato/vpp-agent/plugins/linuxplugin/model/interfaces"

	"strings"

	"github.com/ligato/cn-infra/servicelabel"
	"github.com/ligato/cn-infra/utils/addrs"
)

/* how often in seconds to refresh the microservice label -> docker container PID map */
const dockerRefreshPeriod = 3 * time.Second

// LinuxInterfaceConfig is used to cache the configuration of Linux interfaces.
type LinuxInterfaceConfig struct {
	config   *intf.LinuxInterfaces_Interface
	vethPeer *LinuxInterfaceConfig
}

// Microservice is used to store PID and ID of the container running a given microservice.
type Microservice struct {
	label string
	pid   int
	id    string
}

// unavailableMicroserviceErr is error implementation used when a given microservice is not deployed.
type unavailableMicroserviceErr struct {
	label string
}

func (e *unavailableMicroserviceErr) Error() string {
	return fmt.Sprintf("Microservice '%s' is not available", e.label)
}

// LinuxInterfaceConfigurator runs in the background in its own goroutine where it watches for any changes
// in the configuration of interfaces as modelled by the proto file "model/interfaces/interfaces.proto"
// and stored in ETCD under the key "/vnf-agent/{vnf-agent}/linux/config/v1/interface".
// Updates received from the northbound API are compared with the Linux network configuration and differences
// are applied through the Netlink API.
type LinuxInterfaceConfigurator struct {
	cfgLock sync.Mutex

	/* logical interface name -> Linux interface index (both managed and unmanaged interfaces) */
	ifIndexes idxvpp.NameToIdxRW

	/* interface caches (managed interfaces only) */
	intfByName          map[string]*LinuxInterfaceConfig   /* interface name -> interface configuration */
	intfsByMicroservice map[string][]*LinuxInterfaceConfig /* microservice label -> list of interfaces attached to this microservice */

	/* microservice caches */
	microserviceByLabel map[string]*Microservice /* microservice label -> microservice info */
	microserviceByID    map[string]*Microservice /* microservice container ID -> microservice info */

	/* docker client - used to convert microservice label into the PID and ID of the container */
	dockerClient *docker.Client

	/* management of go routines */
	cancel context.CancelFunc // cancel can be used to cancel all goroutines and their jobs inside of the plugin
	wg     sync.WaitGroup     // wait group that allows to wait until all goroutines of the plugin have finished
}

// Init linuxplugin and start go routines
func (plugin *LinuxInterfaceConfigurator) Init(ifIndexes idxvpp.NameToIdxRW) error {
	log.Debug("Initializing LinuxInterfaceConfigurator")
	plugin.ifIndexes = ifIndexes

	// allocate caches
	plugin.intfByName = make(map[string]*LinuxInterfaceConfig)
	plugin.intfsByMicroservice = make(map[string][]*LinuxInterfaceConfig)
	plugin.microserviceByLabel = make(map[string]*Microservice)
	plugin.microserviceByID = make(map[string]*Microservice)

	var err error
	plugin.dockerClient, err = docker.NewClientFromEnv()
	if err != nil || plugin.dockerClient == nil {
		log.Warn("Failed to connect with the docker daemon. Will keep re-connecting in the background.")
	}

	var ctx context.Context
	ctx, plugin.cancel = context.WithCancel(context.Background())
	go plugin.trackMicroservices(ctx)

	return nil
}

// Close stops all goroutines started by linuxplugin
func (plugin *LinuxInterfaceConfigurator) Close() error {
	plugin.cancel()
	plugin.wg.Wait()

	return nil
}

// Resync configures an initial set of interfaces. Existing Linux interfaces are registered and potentially re-configured.
func (plugin *LinuxInterfaceConfigurator) Resync(interfaces []*intf.LinuxInterfaces_Interface) error {
	var wasError error
	log.WithField("cfg", plugin).Debug("RESYNC Interface begin.")

	// Step 1: Dump actual state of the Linux network stack (subset of it)
	err := plugin.LookupLinuxInterfaces(nil)
	if err != nil {
		return err
	}

	// Step 2: Create missing Linux interfaces and recreate existing ones
	for _, iface := range interfaces {
		err := plugin.ConfigureLinuxInterface(iface)
		if err != nil {
			wasError = err
		}
	}

	log.WithField("cfg", plugin).Debug("RESYNC Interface end. ", wasError)

	return wasError
}

// LookupLinuxInterfaces looks up all Linux interfaces in the current namespace and registers them into the name-to-index
// mapping.
func (plugin *LinuxInterfaceConfigurator) LookupLinuxInterfaces(ns *intf.LinuxInterfaces_Interface_Namespace) error {
	intfs, err := net.Interfaces()
	if err != nil {
		return err
	}
	for _, intf := range intfs {
		idx := GetLinuxInterfaceIndex(intf.Name)
		if idx < 0 {
			continue
		}
		ifType, err := linuxcalls.GetInterfaceType(intf.Name)
		if err != nil {
			continue
		}
		if ns != nil && ifType != "veth" {
			/* skip non-VETH interfaces from other namespaces */
			continue
		}
		log.WithFields(log.Fields{"name": intf.Name, "idx": idx}).Debug("Found new Linux interface")
		plugin.ifIndexes.RegisterName(intf.Name, uint32(idx), ns)
	}
	return nil
}

// ConfigureLinuxInterface reacts to a new northbound Linux interface config by creating and configuring
// the interface in the host network stack through Netlink API.
func (plugin *LinuxInterfaceConfigurator) ConfigureLinuxInterface(iface *intf.LinuxInterfaces_Interface) error {
	log.Println("Configuring Linux interface", iface.Name)
	var err error

	if iface.Type != intf.LinuxInterfaces_VETH {
		return errors.New("unsupported Linux interface type")
	}

	plugin.cfgLock.Lock()
	defer plugin.cfgLock.Unlock()

	if _, exists := plugin.intfByName[iface.Name]; exists {
		return fmt.Errorf("VETH interface %s is already configured", iface.Name)
	}

	peer := plugin.getInterfaceConfig(iface.Veth.PeerIfName)
	config := plugin.addToCache(iface, peer)

	// create only after both ends are configured and target namespaces are available
	if !plugin.isNamespaceAvailable(iface.Namespace) || peer == nil || !plugin.isNamespaceAvailable(peer.config.Namespace) {
		log.WithField("ifName", iface.Name).Debug("VETH interface is not ready to be configured")
		return nil
	}

	nsMgmtCtx := linuxcalls.NewNamespaceMgmtCtx()
	err = plugin.addVethInterface(nsMgmtCtx, iface, peer.config)
	if err != nil {
		return err
	}

	err = plugin.configureLinuxInterface(nsMgmtCtx, config)
	if err != nil {
		return err
	}
	return plugin.configureLinuxInterface(nsMgmtCtx, peer)
}

func (plugin *LinuxInterfaceConfigurator) configureLinuxInterface(nsMgmtCtx *linuxcalls.NamespaceMgmtCtx, iface *LinuxInterfaceConfig) error {
	var err error

	idx := GetLinuxInterfaceIndex(iface.config.Name)
	if idx < 0 {
		return fmt.Errorf("failed to get index of the VETH interface %s", iface.config.Name)
	}

	// move interface to the proper namespace
	ns := iface.config.Namespace
	if ns != nil && ns.Type == intf.LinuxInterfaces_Interface_Namespace_MICROSERVICE_REF_NS {
		ns = plugin.convertMicroserviceNsToPidNs(ns.Microservice)
		if ns == nil {
			return &unavailableMicroserviceErr{}
		}
	}
	err = linuxcalls.SetInterfaceNamespace(nsMgmtCtx, iface.config.Name, ns)
	if err != nil {
		return fmt.Errorf("failed to move interface across namespaces: %v", err)
	}

	// continue configuring interface in its namespace
	revertNs, err := plugin.switchToNamespace(nsMgmtCtx, iface.config.Namespace)
	if err != nil {
		return fmt.Errorf("failed to switch network namespace: %v", err)
	}
	defer revertNs()

	// set interface up
	if iface.config.Enabled {
		err := linuxcalls.InterfaceAdminUp(iface.config.Name)
		if nil != err {
			return fmt.Errorf("failed to enable Linux interface: %v", err)
		}
	}

	var wasError error

	// configure optional mac address
	if iface.config.PhysAddress != "" {
		log.WithFields(log.Fields{"PhysAddress": iface.config.PhysAddress, "ifName": iface.config.Name}).Debug("MAC address configured.")
		err := linuxcalls.SetInterfaceMac(iface.config.Name, iface.config.PhysAddress)
		if err != nil {
			wasError = fmt.Errorf("failed to assign physical address to Linux interface: %v", err)
		}
	}

	// configure all the ip addresses
	newAddrs, err := addrs.StrAddrsToStruct(iface.config.IpAddresses)
	if err != nil {
		return err
	}
	for i := range newAddrs {
		log.WithFields(log.Fields{"IPaddress": newAddrs[i], "ifName": iface.config.Name}).Debug("IP address added.")
		err := linuxcalls.AddInterfaceIP(iface.config.Name, newAddrs[i])
		if nil != err {
			wasError = fmt.Errorf("failed to assign IPv4 address to Linux interface: %v", err)
		}
	}

	// configure MTU
	mtu := iface.config.Mtu
	if mtu > 0 {
		log.WithFields(log.Fields{"MTU": mtu, "ifName": iface.config.Name}).Debug("MTU configured.")
		err := linuxcalls.SetInterfaceMTU(iface.config.Name, int(mtu))
		if nil != err {
			wasError = fmt.Errorf("failed to set MTU of a Linux interface: %v", err)
		}
	}

	plugin.ifIndexes.RegisterName(iface.config.Name, uint32(idx), iface.config.Namespace)
	log.WithFields(log.Fields{"ifName": iface.config.Name, "ifIdx": idx}).Debug("An entry added into ifState.")

	return wasError
}

// ModifyLinuxInterface applies changes in the NB configuration of a Linux interface into the host network stack
// through Netlink API.
func (plugin *LinuxInterfaceConfigurator) ModifyLinuxInterface(newConfig *intf.LinuxInterfaces_Interface,
	oldConfig *intf.LinuxInterfaces_Interface) error {

	log.Println("'Modifying' Linux interface", newConfig.Name)
	var err error
	var ifName = newConfig.Name

	if newConfig == nil {
		return errors.New("newConfig is null")
	}
	if oldConfig == nil {
		return errors.New("oldConfig is null")
	}

	if newConfig.Type != intf.LinuxInterfaces_VETH {
		return errors.New("unsupported Linux interface type")
	}

	if newConfig.Veth.PeerIfName != oldConfig.Veth.PeerIfName ||
		linuxcalls.CompareNamespaces(newConfig.Namespace, oldConfig.Namespace) != 0 {
		// change of the peer interface or the namespace requires to create the interface from the scratch
		err := plugin.DeleteLinuxInterface(oldConfig)
		if err == nil {
			err = plugin.ConfigureLinuxInterface(newConfig)
		}
		return err
	}

	plugin.cfgLock.Lock()
	defer plugin.cfgLock.Unlock()

	// update the cached configuration
	plugin.removeFromCache(oldConfig)
	peer := plugin.getInterfaceConfig(newConfig.Veth.PeerIfName)
	plugin.addToCache(newConfig, peer)

	if !plugin.isNamespaceAvailable(newConfig.Namespace) || peer == nil || !plugin.isNamespaceAvailable(peer.config.Namespace) {
		// interface doesn't actually exist physically
		log.WithField("ifName", ifName).Debug("VETH interface is not ready to be re-configured")
		return nil
	}

	// reconfigure interface in its namespace
	nsMgmtCtx := linuxcalls.NewNamespaceMgmtCtx()
	revertNs, err := plugin.switchToNamespace(nsMgmtCtx, oldConfig.Namespace)
	if err != nil {
		return fmt.Errorf("failed to switch network namespace: %v", err)
	}
	defer revertNs()

	// verify that the interface exists in the Linux namespace
	idx := GetLinuxInterfaceIndex(ifName)
	if idx < 0 {
		log.WithFields(log.Fields{"ifName": ifName}).Debug("Linux interface not found.")
		return nil
	}

	var wasError error

	// admin status
	if newConfig.Enabled != oldConfig.Enabled {
		if newConfig.Enabled {
			err = linuxcalls.InterfaceAdminUp(ifName)
		} else {
			err = linuxcalls.InterfaceAdminDown(ifName)
		}
		if nil != err {
			wasError = fmt.Errorf("failed to enable/disable Linux interface: %v", err)
		}
	}

	// configure new mac address if set
	if newConfig.PhysAddress != "" && newConfig.PhysAddress != oldConfig.PhysAddress {
		log.WithFields(log.Fields{"PhysAddress": newConfig.PhysAddress, "ifName": ifName}).Debug("MAC address re-configured.")
		err := linuxcalls.SetInterfaceMac(ifName, newConfig.PhysAddress)
		if err != nil {
			wasError = fmt.Errorf("failed to assign physical address to a Linux interface: %v", err)
		}
	}

	// ip addresses
	newAddrs, err := addrs.StrAddrsToStruct(newConfig.IpAddresses)
	if err != nil {
		return err
	}
	oldAddrs, err := addrs.StrAddrsToStruct(oldConfig.IpAddresses)
	if err != nil {
		return err
	}
	var del, add []*net.IPNet

	del, add = addrs.DiffAddr(newAddrs, oldAddrs)

	for i := range del {
		log.WithFields(log.Fields{"IPaddress": del[i], "ifName": ifName}).Debug("IP address deleted.")
		err := linuxcalls.DelInterfaceIP(ifName, del[i])
		if nil != err {
			wasError = fmt.Errorf("failed to unassign IPv4 address from a Linux interface: %v", err)
		}
	}

	for i := range add {
		log.WithFields(log.Fields{"IPaddress": add[i], "ifName": ifName}).Debug("IP address added.")
		err := linuxcalls.AddInterfaceIP(ifName, add[i])
		if nil != err {
			wasError = fmt.Errorf("failed to assign IPv4 address to a Linux interface: %v", err)
		}
	}

	// MTU
	if newConfig.Mtu != oldConfig.Mtu {
		mtu := newConfig.Mtu
		if mtu > 0 {
			log.WithFields(log.Fields{"MTU": mtu, "ifName": ifName}).Debug("MTU re-configured.")
			err := linuxcalls.SetInterfaceMTU(ifName, int(mtu))
			if nil != err {
				wasError = fmt.Errorf("failed to set MTU of a Linux interface: %v", err)
			}
		}
	}

	return wasError
}

// DeleteLinuxInterface reacts to a removed NB configuration of a Linux interface.
func (plugin *LinuxInterfaceConfigurator) DeleteLinuxInterface(iface *intf.LinuxInterfaces_Interface) error {
	log.Println("'Deleting' Linux interface", iface.Name)

	if iface.Type != intf.LinuxInterfaces_VETH {
		return errors.New("unsupported Linux interface type")
	}

	plugin.cfgLock.Lock()
	defer plugin.cfgLock.Unlock()

	oldCfg := plugin.removeFromCache(iface)
	peer := oldCfg.vethPeer

	if !plugin.isNamespaceAvailable(oldCfg.config.Namespace) || peer == nil || !plugin.isNamespaceAvailable(peer.config.Namespace) {
		log.WithField("ifName", oldCfg.config.Name).Debug("VETH interface already physically doesn't exist")
		return nil
	}

	// Move to the namespace with the interface.
	nsMgmtCtx := linuxcalls.NewNamespaceMgmtCtx()
	revertNs, err := plugin.switchToNamespace(nsMgmtCtx, oldCfg.config.Namespace)
	if err != nil {
		return fmt.Errorf("failed to switch network namespace: %v", err)
	}
	defer revertNs()

	err = linuxcalls.DelVethInterface(oldCfg.config.Name, peer.config.Name)
	if err != nil {
		return fmt.Errorf("failed to delete VETH interface: %v", err)
	}

	// Unregister both VETH ends from the in-memory map (following triggers notifications for all subscribers).
	plugin.ifIndexes.UnregisterName(iface.Name)
	plugin.ifIndexes.UnregisterName(peer.config.Name)
	return nil
}

// removeObsoleteVeth deletes VETH interface which should no longer exist.
func (plugin *LinuxInterfaceConfigurator) removeObsoleteVeth(nsMgmtCtx *linuxcalls.NamespaceMgmtCtx, vethName string) error {
	log.WithField("vethName", vethName).Debug("Attempting to remove obsolete VETH")

	_, meta, exists := plugin.ifIndexes.LookupIdx(vethName)
	if exists {
		ns := meta.(*intf.LinuxInterfaces_Interface_Namespace)
		revertNs, err := plugin.switchToNamespace(nsMgmtCtx, ns)
		defer revertNs()
		if err != nil {
			// already removed as namespace no longer exists
			plugin.ifIndexes.UnregisterName(vethName)
			return nil
		}
		exists, err := linuxcalls.InterfaceExists(vethName)
		if err != nil {
			return err
		}
		if !exists {
			// already removed
			plugin.ifIndexes.UnregisterName(vethName)
			return nil
		}
		ifType, err := linuxcalls.GetInterfaceType(vethName)
		if err != nil {
			return err
		}
		if ifType != "veth" {
			return fmt.Errorf("interface '%s' already exists and is not VETH", vethName)
		}
		peerName, err := linuxcalls.GetVethPeerName(vethName)
		if err != nil {
			return err
		}
		log.WithFields(log.Fields{"ifName": vethName, "peerName": peerName}).Debug("Removing obsolete VETH interface")
		err = linuxcalls.DelVethInterface(vethName, peerName)
		if err != nil {
			return err
		}
		plugin.ifIndexes.UnregisterName(vethName)
		plugin.ifIndexes.UnregisterName(peerName)
	}
	return nil
}

// addVethInterface creates a new VETH interface with a "clean" configuration.
func (plugin *LinuxInterfaceConfigurator) addVethInterface(nsMgmtCtx *linuxcalls.NamespaceMgmtCtx, iface *intf.LinuxInterfaces_Interface, peer *intf.LinuxInterfaces_Interface) error {
	err := plugin.removeObsoleteVeth(nsMgmtCtx, iface.Name)
	if err != nil {
		return err
	}
	err = plugin.removeObsoleteVeth(nsMgmtCtx, peer.Name)
	if err != nil {
		return err
	}
	err = linuxcalls.AddVethInterface(iface.Name, peer.Name)
	if err != nil {
		return fmt.Errorf("failed to create new VETH: %v", err)
	}
	return nil
}

// getInterfaceConfig returns cached configuration of a given interface.
func (plugin *LinuxInterfaceConfigurator) getInterfaceConfig(ifName string) *LinuxInterfaceConfig {
	config, ok := plugin.intfByName[ifName]
	if ok {
		return config
	}
	return nil
}

// addToCache adds interface configuration into the cache.
func (plugin *LinuxInterfaceConfigurator) addToCache(iface *intf.LinuxInterfaces_Interface, peer *LinuxInterfaceConfig) *LinuxInterfaceConfig {
	config := &LinuxInterfaceConfig{config: iface, vethPeer: peer}
	plugin.intfByName[iface.Name] = config
	if peer != nil {
		peer.vethPeer = config
	}
	if iface.Namespace != nil && iface.Namespace.Type == intf.LinuxInterfaces_Interface_Namespace_MICROSERVICE_REF_NS {
		if _, ok := plugin.intfsByMicroservice[iface.Namespace.Microservice]; ok {
			plugin.intfsByMicroservice[iface.Namespace.Microservice] = append(plugin.intfsByMicroservice[iface.Namespace.Microservice], config)
		} else {
			plugin.intfsByMicroservice[iface.Namespace.Microservice] = []*LinuxInterfaceConfig{config}
		}
	}
	log.Debugf("Linux interface with name %v added to cache (peer: %v)",
		iface.Name, peer)
	return config
}

// removeFromCache removes interfaces configuration from the cache.
func (plugin *LinuxInterfaceConfigurator) removeFromCache(iface *intf.LinuxInterfaces_Interface) *LinuxInterfaceConfig {
	if config, ok := plugin.intfByName[iface.Name]; ok {
		if config.vethPeer != nil {
			config.vethPeer.vethPeer = nil
		}
		if iface.Namespace != nil && iface.Namespace.Type == intf.LinuxInterfaces_Interface_Namespace_MICROSERVICE_REF_NS {
			filtered := []*LinuxInterfaceConfig{}
			for _, intf := range plugin.intfsByMicroservice[iface.Namespace.Microservice] {
				if intf.config.Name != iface.Name {
					filtered = append(filtered, intf)
				}
			}
			plugin.intfsByMicroservice[iface.Namespace.Microservice] = filtered
		}
		delete(plugin.intfByName, iface.Name)
		log.Debugf("Linux interface with name %v was removed from cache", iface.Name)
		return config
	}
	return nil
}

// isNamespaceAvailable return true if the destination namespace is available.
func (plugin *LinuxInterfaceConfigurator) isNamespaceAvailable(ns *intf.LinuxInterfaces_Interface_Namespace) bool {

	if ns != nil && ns.Type == intf.LinuxInterfaces_Interface_Namespace_MICROSERVICE_REF_NS {
		if plugin.dockerClient == nil {
			return false
		}
		_, available := plugin.microserviceByLabel[ns.Microservice]
		return available
	}
	return true
}

// convertMicroserviceNsToPidNs converts microservice-referenced namespace into the PID-referenced namespace.
func (plugin *LinuxInterfaceConfigurator) convertMicroserviceNsToPidNs(microserviceLabel string) (pidNs *intf.LinuxInterfaces_Interface_Namespace) {

	if microservice, ok := plugin.microserviceByLabel[microserviceLabel]; ok {
		pidNamespace := &intf.LinuxInterfaces_Interface_Namespace{}
		pidNamespace.Type = intf.LinuxInterfaces_Interface_Namespace_PID_REF_NS
		pidNamespace.Pid = uint32(microservice.pid)
		return pidNamespace
	}
	return nil
}

// switchToNamespace switches the network namespace of the current thread.
func (plugin *LinuxInterfaceConfigurator) switchToNamespace(nsMgmtCtx *linuxcalls.NamespaceMgmtCtx, ns *intf.LinuxInterfaces_Interface_Namespace) (revert func(), err error) {

	if ns != nil && ns.Type == intf.LinuxInterfaces_Interface_Namespace_MICROSERVICE_REF_NS {
		ns = plugin.convertMicroserviceNsToPidNs(ns.Microservice)
		if ns == nil {
			return func() {}, &unavailableMicroserviceErr{}
		}
	}
	return linuxcalls.SwitchNamespace(nsMgmtCtx, ns)
}

// trackMicroservices is running in the background and maintains a map of microservice labels to container info.
func (plugin *LinuxInterfaceConfigurator) trackMicroservices(ctx context.Context) {
	var err error
	var since string
	var lastInspected int64

	created := []string{}       // IDs of containers in the state "created"
	microservices := []string{} // IDs of containers running microservices

	plugin.wg.Add(1)
	defer plugin.wg.Done()

	nsMgmtCtx := linuxcalls.NewNamespaceMgmtCtx()

	for {
		var newest int64
		var listOpts docker.ListContainersOptions
		var containers []docker.APIContainers
		nextCreated := []string{}
		nextMicroservices := []string{}

		if plugin.dockerClient == nil {
			plugin.dockerClient, err = docker.NewClientFromEnv()
			if err == nil {
				log.Info("Successfully established connection with the docker daemon.")
			} else {
				goto nextRefresh
			}
		}

		// first check if any microservice has terminated
		for _, container := range microservices {
			details, err := plugin.dockerClient.InspectContainer(container)
			if err != nil || !details.State.Running {
				plugin.processTerminatedMicroservice(nsMgmtCtx, container)
			} else {
				nextMicroservices = append(nextMicroservices, container)
			}
		}
		microservices = nextMicroservices

		// now check if previously created containers have transitioned to the state "running"
		for _, container := range created {
			details, err := plugin.dockerClient.InspectContainer(container)
			if err == nil {
				if details.State.Running {
					if plugin.detectMicroservice(nsMgmtCtx, details) {
						microservices = append(microservices, container)
					}
				} else if details.State.Status == "created" {
					nextCreated = append(nextCreated, container)
				}
			}
		}
		created = nextCreated

		// finally inspect newly created containers
		listOpts = docker.ListContainersOptions{}
		listOpts.All = true
		listOpts.Filters = map[string][]string{}
		if since != "" {
			listOpts.Filters["since"] = []string{since}
		}

		containers, err = plugin.dockerClient.ListContainers(listOpts)
		if err != nil {
			if err, ok := err.(*docker.Error); ok && err.Status == 404 {
				since = ""
			}
			goto nextRefresh
		}

		for _, container := range containers {
			if container.State == "running" && container.Created > lastInspected {
				// inspect the container to get the list of defined environment variables
				details, err := plugin.dockerClient.InspectContainer(container.ID)
				if err != nil {
					continue
				}
				if plugin.detectMicroservice(nsMgmtCtx, details) {
					microservices = append(microservices, container.ID)
				}
			}
			if container.State == "created" {
				created = append(created, container.ID)
			}
			if container.Created > newest {
				newest = container.Created
				since = container.ID
			}
		}

		if newest > lastInspected {
			lastInspected = newest
		}

	nextRefresh:
		// sleep before another refresh
		select {
		case <-time.After(dockerRefreshPeriod):
			continue
		case <-ctx.Done():
			return
		}
	}
}

// detectMicroservice inspects container to see if it is a microservice.
// If microservice is detected, processNewMicroservice() is called to process it.
func (plugin *LinuxInterfaceConfigurator) detectMicroservice(nsMgmtCtx *linuxcalls.NamespaceMgmtCtx, container *docker.Container) bool {
	// search for the microservice label
	var label string
	for _, env := range container.Config.Env {
		if strings.HasPrefix(env, servicelabel.MicroserviceLabelEnvVar+"=") {
			label = env[len(servicelabel.MicroserviceLabelEnvVar)+1:]
			if label != "" {
				plugin.processNewMicroservice(nsMgmtCtx, label, container.ID, container.State.Pid)
				return true
			}
		}
	}
	return false
}

// processNewMicroservice is triggered every time a new microservice gets freshly started. All pending interfaces are moved
// to its namespace.
func (plugin *LinuxInterfaceConfigurator) processNewMicroservice(nsMgmtCtx *linuxcalls.NamespaceMgmtCtx, microserviceLabel string, id string, pid int) {
	plugin.cfgLock.Lock()
	defer plugin.cfgLock.Unlock()

	_, exists := plugin.microserviceByLabel[microserviceLabel]
	if exists {
		log.WithFields(log.Fields{"label": microserviceLabel}).Warn("Unexpectedly rediscovered microservice")
		return
	}
	log.WithFields(log.Fields{"label": microserviceLabel, "pid": pid, "id": id}).Debug("Discovered new microservice")

	microservice := &Microservice{label: microserviceLabel, pid: pid, id: id}
	plugin.microserviceByLabel[microserviceLabel] = microservice
	plugin.microserviceByID[id] = microservice

	// dump all interfaces in this newly discovered namespace
	ns := &intf.LinuxInterfaces_Interface_Namespace{}
	ns.Type = intf.LinuxInterfaces_Interface_Namespace_MICROSERVICE_REF_NS
	ns.Microservice = microserviceLabel
	revertNs, err := plugin.switchToNamespace(nsMgmtCtx, ns)
	if err == nil {
		plugin.LookupLinuxInterfaces(ns)
	} else {
		log.WithFields(log.Fields{"microservice": microserviceLabel, "err": err}).Warn(
			"Failed to switch into the namespace of a microservice")
	}
	revertNs()

	if interfaces, ok := plugin.intfsByMicroservice[microserviceLabel]; ok {
		skip := make(map[string]struct{}) /* interfaces to be skipped in subsequent iterations */
		for _, intf := range interfaces {
			if _, toSkip := skip[intf.config.Name]; toSkip {
				continue
			}
			peer := intf.vethPeer
			if peer != nil {
				// peer will be processed in this iteration and skipped in the subsequent ones
				skip[peer.config.Name] = struct{}{}
			}
			if peer != nil && plugin.isNamespaceAvailable(peer.config.Namespace) {
				// VETH is ready to be created and configured
				err := plugin.addVethInterface(nsMgmtCtx, intf.config, peer.config)
				if err != nil {
					log.Warn(err.Error())
					continue
				}
				err = plugin.configureLinuxInterface(nsMgmtCtx, intf)
				if err == nil {
					err = plugin.configureLinuxInterface(nsMgmtCtx, peer)
				}
				if err != nil {
					log.Warn("failed to configure VETH: %v", err)
				}
			}
		}
	}
}

// processTerminatedMicroservice is triggered every time a known microservice has terminated. All associated interfaces
// become obsolete and are thus removed.
func (plugin *LinuxInterfaceConfigurator) processTerminatedMicroservice(nsMgmtCtx *linuxcalls.NamespaceMgmtCtx, id string) {
	plugin.cfgLock.Lock()
	defer plugin.cfgLock.Unlock()

	microservice, exists := plugin.microserviceByID[id]
	if !exists {
		log.WithFields(log.Fields{"id": id}).Warn("Detected removal of an unknown microservice")
		return
	}
	log.WithFields(log.Fields{"label": microservice.label, "pid": microservice.pid, "id": microservice.id}).Debug(
		"Microservice has terminated")

	delete(plugin.microserviceByLabel, microservice.label)
	delete(plugin.microserviceByID, microservice.id)

	if interfaces, ok := plugin.intfsByMicroservice[microservice.label]; ok {
		for _, intf := range interfaces {
			plugin.removeObsoleteVeth(nsMgmtCtx, intf.config.Name)
			plugin.removeObsoleteVeth(nsMgmtCtx, intf.vethPeer.config.Name)
		}
	}
	// TODO: unregister unmanaged interfaces
}
