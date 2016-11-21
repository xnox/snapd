// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2016 Canonical Ltd
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

// Package udev implements integration between snappy, udev and
// ubuntu-core-laucher around tagging character and block devices so that they
// can be accessed by applications.
//
// TODO: Document this better
package udev

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/snapcore/snapd/dirs"
	"github.com/snapcore/snapd/interfaces"
	"github.com/snapcore/snapd/osutil"
	"github.com/snapcore/snapd/snap"
)

// Backend is responsible for maintaining udev rules.
type Backend struct{}

// Name returns the name of the backend.
func (b *Backend) Name() string {
	return "udev"
}

// snapRulesFileName returns the path of the snap udev rules file.
func snapRulesFilePath(snapName string) string {
	rulesFileName := fmt.Sprintf("70-%s.rules", snap.SecurityTag(snapName))
	return filepath.Join(dirs.SnapUdevRulesDir, rulesFileName)
}

// Setup creates udev rules specific to a given snap.
// If any of the rules are changed or removed then udev database is reloaded.
//
// Udev has no concept of a complain mode so confinment type is ignored.
//
// If the method fails it should be re-tried (with a sensible strategy) by the caller.
func (b *Backend) Setup(snapInfo *snap.Info, confinement snap.ConfinementType, repo *interfaces.Repository) error {
	snapName := snapInfo.Name()
	snippets, err := repo.SecuritySnippetsForSnap(snapInfo.Name(), interfaces.SecurityUDev)
	if err != nil {
		return fmt.Errorf("cannot obtain udev security snippets for snap %q: %s", snapName, err)
	}
	content, err := b.combineSnippets(snapInfo, snippets)
	if err != nil {
		return fmt.Errorf("cannot obtain expected udev rules for snap %q: %s", snapName, err)
	}
	dir := dirs.SnapUdevRulesDir
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("cannot create directory for udev rules %q: %s", dir, err)
	}

	rulesFilePath := snapRulesFilePath(snapInfo.Name())

	if len(content) == 0 {
		// Make sure that the rules file gets removed when we don't have any
		// content and exists.
		err = os.Remove(rulesFilePath)
		if err != nil && !os.IsNotExist(err) {
			return err
		} else if err == nil {
			return ReloadRules()
		}
		return nil
	}

	var buffer bytes.Buffer
	buffer.WriteString("# This file is automatically generated.\n")
	for _, snippet := range content {
		buffer.Write(snippet)
		buffer.WriteByte('\n')
	}

	rulesFileState := &osutil.FileState{
		Content: buffer.Bytes(),
		Mode:    0644,
	}

	// EnsureFileState will make sure the file will be only updated when its content
	// has changed and will otherwise return an error which prevents us from reloading
	// udev rules when not needed.
	err = osutil.EnsureFileState(rulesFilePath, rulesFileState)
	if err == osutil.ErrSameState {
		return nil
	} else if err != nil {
		return err
	}

	return ReloadRules()
}

// Remove removes udev rules specific to a given snap.
// If any of the rules are removed then udev database is reloaded.
//
// This method should be called after removing a snap.
//
// If the method fails it should be re-tried (with a sensible strategy) by the caller.
func (b *Backend) Remove(snapName string) error {
	rulesFilePath := snapRulesFilePath(snapName)
	err := os.Remove(rulesFilePath)
	if os.IsNotExist(err) {
		// If file doesn't exist we avoid reloading the udev rules when we return here
		return nil
	} else if err != nil {
		return err
	}
	return ReloadRules()
}

// combineSnippets combines security snippets collected from all the interfaces
// affecting a given snap into a content map applicable to EnsureDirState.
func (b *Backend) combineSnippets(snapInfo *snap.Info, snippets map[string][][]byte) (result [][]byte, err error) {
	var snapSnippets = make(map[string][]byte)

	// We put all snippets from apps and hooks in the following part in a
	// map to reach a deduplicated set of snippets we can then write out
	// in a per snap udev rules file.

	for _, appInfo := range snapInfo.Apps {
		securityTag := appInfo.SecurityTag()
		appSnippets := snippets[securityTag]
		if len(appSnippets) == 0 {
			continue
		}

		for _, snippet := range appSnippets {
			snapSnippets[string(snippet)] = snippet
		}
	}

	for _, hookInfo := range snapInfo.Hooks {
		securityTag := hookInfo.SecurityTag()
		hookSnippets := snippets[securityTag]
		if len(hookSnippets) == 0 {
			continue
		}

		for _, snippet := range hookSnippets {
			snapSnippets[string(snippet)] = snippet
		}
	}

	nonePrefix := snap.NoneSecurityTag(snapInfo.Name(), "")
	for securityTag, slotSnippets := range snippets {
		if !strings.HasPrefix(securityTag, nonePrefix) {
			continue
		}

		for _, snippet := range slotSnippets {
			snapSnippets[string(snippet)] = snippet
		}
	}

	var combinedSnippets [][]byte
	for _, snippet := range snapSnippets {
		combinedSnippets = append(combinedSnippets, snippet)
	}

	return combinedSnippets, nil
}
