// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2016-2017 Canonical Ltd
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License version 3 as
 * published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package ifacestate

import (
	"time"

	"github.com/snapcore/snapd/interfaces"
	"github.com/snapcore/snapd/interfaces/backends"
	"github.com/snapcore/snapd/logger"
	"github.com/snapcore/snapd/overlord/hookstate"
	"github.com/snapcore/snapd/overlord/ifacestate/ifacerepo"
	"github.com/snapcore/snapd/overlord/ifacestate/udevmonitor"
	"github.com/snapcore/snapd/overlord/state"
	"github.com/snapcore/snapd/timings"
)

type deviceData struct{ ifaceName, hotplugKey string }

// InterfaceManager is responsible for the maintenance of interfaces in
// the system state.  It maintains interface connections, and also observes
// installed snaps to track the current set of available plugs and slots.
type InterfaceManager struct {
	state *state.State
	repo  *interfaces.Repository

	udevMon             udevmonitor.Interface
	udevRetryTimeout    time.Time
	udevMonitorDisabled bool
	// indexed by interface name and device key. Reset to nil when enumeration is done.
	enumeratedDeviceKeys map[string]map[string]bool
	enumerationDone      bool
	// maps sysfs path -> [(interface name, device key)...]
	hotplugDevicePaths map[string][]deviceData
}

// Manager returns a new InterfaceManager.
// Extra interfaces can be provided for testing.
func Manager(s *state.State, hookManager *hookstate.HookManager, runner *state.TaskRunner, extraInterfaces []interfaces.Interface, extraBackends []interfaces.SecurityBackend) (*InterfaceManager, error) {
	delayedCrossMgrInit()

	perfTimings := timings.New(map[string]string{"startup": "ifacemgr"})

	// NOTE: hookManager is nil only when testing.
	if hookManager != nil {
		setupHooks(hookManager)
	}

	// Leave udevRetryTimeout at the default value, so that udev is initialized on first Ensure run.
	m := &InterfaceManager{
		state: s,
		repo:  interfaces.NewRepository(),
		// note: enumeratedDeviceKeys is reset to nil when enumeration is done
		enumeratedDeviceKeys: make(map[string]map[string]bool),
		hotplugDevicePaths:   make(map[string][]deviceData),
	}

	if err := m.initialize(extraInterfaces, extraBackends, perfTimings); err != nil {
		return nil, err
	}

	s.Lock()
	ifacerepo.Replace(s, m.repo)
	s.Unlock()

	taskKinds := map[string]bool{}
	addHandler := func(kind string, do, undo state.HandlerFunc) {
		taskKinds[kind] = true
		runner.AddHandler(kind, do, undo)
	}

	addHandler("connect", m.doConnect, m.undoConnect)
	addHandler("disconnect", m.doDisconnect, m.undoDisconnect)
	addHandler("setup-profiles", m.doSetupProfiles, m.undoSetupProfiles)
	addHandler("remove-profiles", m.doRemoveProfiles, m.doSetupProfiles)
	addHandler("discard-conns", m.doDiscardConns, m.undoDiscardConns)
	addHandler("auto-connect", m.doAutoConnect, m.undoAutoConnect)
	addHandler("gadget-connect", m.doGadgetConnect, nil)
	addHandler("auto-disconnect", m.doAutoDisconnect, nil)
	addHandler("hotplug-add-slot", m.doHotplugAddSlot, nil)
	addHandler("hotplug-connect", m.doHotplugConnect, nil)
	addHandler("hotplug-update-slot", m.doHotplugUpdateSlot, nil)
	addHandler("hotplug-remove-slot", m.doHotplugRemoveSlot, nil)
	addHandler("hotplug-disconnect", m.doHotplugDisconnect, nil)

	// don't block on hotplug-seq-wait task
	runner.AddHandler("hotplug-seq-wait", m.doHotplugSeqWait, nil)

	// helper for ubuntu-core -> core
	addHandler("transition-ubuntu-core", m.doTransitionUbuntuCore, m.undoTransitionUbuntuCore)

	// interface tasks might touch more than the immediate task target snap, serialize them
	runner.AddBlocked(func(t *state.Task, running []*state.Task) bool {
		if !taskKinds[t.Kind()] {
			return false
		}

		for _, t := range running {
			if taskKinds[t.Kind()] {
				return true
			}
		}

		return false
	})

	s.Lock()
	perftimings.Save(s)
	s.Unlock()

	return m, nil
}

// Ensure implements StateManager.Ensure.
func (m *InterfaceManager) Ensure() error {
	if m.udevMon != nil || m.udevMonitorDisabled {
		return nil
	}

	// don't initialize udev monitor until we have a system snap so that we
	// can attach hotplug interfaces to it.
	if !checkSystemSnapIsPresent(m.state) {
		return nil
	}

	// retry udev monitor initialization every 5 minutes
	now := time.Now()
	if now.After(m.udevRetryTimeout) {
		err := m.initUDevMonitor()
		if err != nil {
			m.udevRetryTimeout = now.Add(udevInitRetryTimeout)
		}
		return err
	}
	return nil
}

// Stop implements StateWaiterStopper.Stop. It stops
// the udev monitor, if running.
func (m *InterfaceManager) Stop() {
	if m.udevMon == nil {
		return
	}
	if err := m.udevMon.Stop(); err != nil {
		logger.Noticef("Cannot stop udev monitor: %s", err)
	}
}

// Repository returns the interface repository used internally by the manager.
//
// This method has two use-cases:
// - it is needed for setting up state in daemon tests
// - it is needed to return the set of known interfaces in the daemon api
//
// In the second case it is only informational and repository has internal
// locks to ensure consistency.
func (m *InterfaceManager) Repository() *interfaces.Repository {
	return m.repo
}

type ConnectionState struct {
	// Auto indicates whether the connection was established automatically
	Auto bool
	// ByGadget indicates whether the connection was trigged by the gadget
	ByGadget bool
	// Interface name of the connection
	Interface string
	// Undesired indicates whether the connection, otherwise established
	// automatically, was explicitly disconnected
	Undesired        bool
	StaticPlugAttrs  map[string]interface{}
	DynamicPlugAttrs map[string]interface{}
	StaticSlotAttrs  map[string]interface{}
	DynamicSlotAttrs map[string]interface{}
	HotplugGone      bool
}

// ConnectionStates return the state of connections tracked by the manager
func (m *InterfaceManager) ConnectionStates() (connStateByRef map[string]ConnectionState, err error) {
	m.state.Lock()
	defer m.state.Unlock()
	states, err := getConns(m.state)
	if err != nil {
		return nil, err
	}

	connStateByRef = make(map[string]ConnectionState, len(states))
	for cref, cstate := range states {
		connStateByRef[cref] = ConnectionState{
			Auto:             cstate.Auto,
			ByGadget:         cstate.ByGadget,
			Interface:        cstate.Interface,
			Undesired:        cstate.Undesired,
			StaticPlugAttrs:  cstate.StaticPlugAttrs,
			DynamicPlugAttrs: cstate.DynamicPlugAttrs,
			StaticSlotAttrs:  cstate.StaticSlotAttrs,
			DynamicSlotAttrs: cstate.DynamicSlotAttrs,
			HotplugGone:      cstate.HotplugGone,
		}
	}
	return connStateByRef, nil
}

// DisableUDevMonitor disables the instantiation of udev monitor, but has no effect
// if udev is already created; it should be called after creating InterfaceManager, before
// first Ensure.
// This method is meant for tests only.
func (m *InterfaceManager) DisableUDevMonitor() {
	if m.udevMon != nil {
		logger.Noticef("UDev Monitor already created, cannot be disabled")
		return
	}
	m.udevMonitorDisabled = true
}

var (
	udevInitRetryTimeout = time.Minute * 5
	createUDevMonitor    = udevmonitor.New
)

func (m *InterfaceManager) initUDevMonitor() error {
	mon := createUDevMonitor(m.hotplugDeviceAdded, m.hotplugDeviceRemoved, m.hotplugEnumerationDone)
	if err := mon.Connect(); err != nil {
		return err
	}
	if err := mon.Run(); err != nil {
		mon.Disconnect()
		return err
	}
	m.udevMon = mon
	return nil
}

// MockSecurityBackends mocks the list of security backends that are used for setting up security.
//
// This function is public because it is referenced in the daemon
func MockSecurityBackends(be []interfaces.SecurityBackend) func() {
	old := backends.All
	backends.All = be
	return func() { backends.All = old }
}

// MockObservedDevicePath adds the given device to the map of observed devices.
// This function is used for tests only.
func (m *InterfaceManager) MockObservedDevicePath(devPath, ifaceName, hotplugKey string) func() {
	old := m.hotplugDevicePaths
	m.hotplugDevicePaths[devPath] = append(m.hotplugDevicePaths[devPath], deviceData{hotplugKey: hotplugKey, ifaceName: ifaceName})
	return func() { m.hotplugDevicePaths = old }
}
