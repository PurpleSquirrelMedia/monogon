// Copyright 2020 The Monogon Project Authors.
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package logtree

import (
	"fmt"
	"runtime"
	"strings"
	"time"
)

// LeveledFor returns a LeveledLogger publishing interface for a given DN. An error may be returned if the DN is
// malformed.
func (l *LogTree) LeveledFor(dn DN) (LeveledLogger, error) {
	return l.nodeByDN(dn)
}

// MustLeveledFor returns a LeveledLogger publishing interface for a given DN, or panics if the given DN is invalid.
func (l *LogTree) MustLeveledFor(dn DN) LeveledLogger {
	leveled, err := l.LeveledFor(dn)
	if err != nil {
		panic(fmt.Errorf("LeveledFor returned: %w", err))
	}
	return leveled
}

// SetVerbosity sets the verbosity for a given DN (non-recursively, ie. for that DN only, not its children).
func (l *LogTree) SetVerbosity(dn DN, level VerbosityLevel) error {
	node, err := l.nodeByDN(dn)
	if err != nil {
		return err
	}
	node.verbosity = level
	return nil
}

// log builds a Payload and entry for a given message, including all related metadata, and appends it to the journal,
// notifying all pertinent subscribers.
func (n *node) log(depth int, severity Severity, msg string) {
	_, file, line, ok := runtime.Caller(2 + depth)
	if !ok {
		file = "???"
		line = 1
	} else {
		slash := strings.LastIndex(file, "/")
		if slash >= 0 {
			file = file[slash+1:]
		}
	}
	p := &Payload{
		timestamp: time.Now(),
		severity:  severity,
		message:   msg,
		file:      file,
		line:      line,
	}
	e := &entry{
		origin:  n.dn,
		payload: p,
	}
	n.tree.journal.append(e)
	n.tree.journal.notify(e)
}

// Info implements the LeveledLogger interface.
func (n *node) Info(args ...interface{}) {
	n.log(0, INFO, fmt.Sprint(args...))
}

// Infof implements the LeveledLogger interface.
func (n *node) Infof(format string, args ...interface{}) {
	n.log(0, INFO, fmt.Sprintf(format, args...))
}

// Warning implements the LeveledLogger interface.
func (n *node) Warning(args ...interface{}) {
	n.log(0, WARNING, fmt.Sprint(args...))
}

// Warningf implements the LeveledLogger interface.
func (n *node) Warningf(format string, args ...interface{}) {
	n.log(0, WARNING, fmt.Sprintf(format, args...))
}

// Error implements the LeveledLogger interface.
func (n *node) Error(args ...interface{}) {
	n.log(0, ERROR, fmt.Sprint(args...))
}

// Errorf implements the LeveledLogger interface.
func (n *node) Errorf(format string, args ...interface{}) {
	n.log(0, ERROR, fmt.Sprintf(format, args...))
}

// Fatal implements the LeveledLogger interface.
func (n *node) Fatal(args ...interface{}) {
	n.log(0, FATAL, fmt.Sprint(args...))
}

// Fatalf implements the LeveledLogger interface.
func (n *node) Fatalf(format string, args ...interface{}) {
	n.log(0, FATAL, fmt.Sprintf(format, args...))
}

// V implements the LeveledLogger interface.
func (n *node) V(v VerbosityLevel) VerboseLeveledLogger {
	return &verbose{
		node:    n,
		enabled: n.verbosity >= v,
	}
}

// verbose implements the VerboseLeveledLogger interface. It is a thin wrapper around node, with an 'enabled' bool. This
// means that V(n)-returned VerboseLeveledLoggers must be short lived, as a changed in verbosity will not affect all
// already existing VerboseLeveledLoggers.
type verbose struct {
	node    *node
	enabled bool
}

func (v *verbose) Enabled() bool {
	return v.enabled
}

func (v *verbose) Info(args ...interface{}) {
	if !v.enabled {
		return
	}
	v.node.log(0, INFO, fmt.Sprint(args...))
}

func (v *verbose) Infof(format string, args ...interface{}) {
	if !v.enabled {
		return
	}
	v.node.log(0, INFO, fmt.Sprintf(format, args...))
}
