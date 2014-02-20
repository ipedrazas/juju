// Copyright 2012, 2013 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package main

import (
	"io/ioutil"
	"path/filepath"

	gc "launchpad.net/gocheck"

	"launchpad.net/juju-core/environs"
	"launchpad.net/juju-core/log/syslog"
	syslogtesting "launchpad.net/juju-core/log/syslog/testing"
	"launchpad.net/juju-core/state"
	coretesting "launchpad.net/juju-core/testing"
	jc "launchpad.net/juju-core/testing/checkers"
	"launchpad.net/juju-core/version"
)

type UpgradeSuite struct {
	commonMachineSuite

	rsyslogPath      string
	machine          *state.Machine
	upgradeToVersion version.Binary
}

var _ = gc.Suite(&UpgradeSuite{})

func fakeRestart() error { return nil }

func (s *UpgradeSuite) SetUpTest(c *gc.C) {
	s.commonMachineSuite.SetUpTest(c)
	s.PatchValue(&syslog.Restart, fakeRestart)

	// As Juju versions increase, update the version to which we are upgrading.
	s.upgradeToVersion = version.Current
	s.upgradeToVersion.Number.Minor++
}

func (s *UpgradeSuite) TestUpgradeStepsStateServer(c *gc.C) {
	s.assertUpgradeSteps(c, state.JobManageEnviron)
	s.assertStateServerUpgrades(c)
}

func (s *UpgradeSuite) TestUpgradeStepsHostMachine(c *gc.C) {
	// We need to first start up a state server.
	ss, _, _ := s.primeAgent(c, s.upgradeToVersion, state.JobManageEnviron)
	a := s.newAgent(c, ss)
	go func() { c.Check(a.Run(nil), gc.IsNil) }()
	defer func() { c.Check(a.Stop(), gc.IsNil) }()
	// Now run the test.
	s.assertUpgradeSteps(c, state.JobHostUnits)
	s.assertHostUpgrades(c)
}

func (s *UpgradeSuite) assertPrepareForUpgrade(c *gc.C) {
	// Prepare for Rsyslog
	syslogDir := c.MkDir()
	s.rsyslogPath = filepath.Join(syslogDir, "rsyslog.conf")
	s.PatchValue(&environs.RsyslogConfPath, s.rsyslogPath)
	err := ioutil.WriteFile(s.rsyslogPath, []byte("hello world"), 0644)
	c.Assert(err, gc.IsNil)
	// Other preparation here as needed...
}

func (s *UpgradeSuite) assertUpgradeSteps(c *gc.C, job state.MachineJob) {
	s.PatchValue(&version.Current, s.upgradeToVersion)
	err := s.State.SetEnvironAgentVersion(s.upgradeToVersion.Number)
	c.Assert(err, gc.IsNil)

	oldVersion := s.upgradeToVersion
	oldVersion.Major = 1
	oldVersion.Minor = 16
	s.machine, _, _ = s.primeAgent(c, oldVersion, job)
	s.assertPrepareForUpgrade(c)

	a := s.newAgent(c, s.machine)
	go func() { c.Check(a.Run(nil), gc.IsNil) }()
	defer func() { c.Check(a.Stop(), gc.IsNil) }()

	// Wait for upgrade steps to run and upgrade worker to start.
	success := false
	for attempt := coretesting.LongAttempt.Start(); attempt.Next(); {
		err = s.machine.Refresh()
		c.Assert(err, gc.IsNil)
		agentTools, err := s.machine.AgentTools()
		c.Assert(err, gc.IsNil)
		success = agentTools.Version == s.upgradeToVersion
		if success {
			break
		}
	}
	// Upgrade worker has completed ok.
	c.Assert(success, jc.IsTrue)
}

func (s *UpgradeSuite) assertStateServerUpgrades(c *gc.C) {
	// Rsyslog
	rsyslogContent := syslogtesting.ExpectedAccumulateSyslogConf(c, s.machine.Tag(), "", 2345)
	s.assertRsyslogUpgrade(c, rsyslogContent)
}

func (s *UpgradeSuite) assertHostUpgrades(c *gc.C) {
	// Rsyslog
	rsyslogContent := syslogtesting.ExpectedForwardSyslogConf(c, s.machine.Tag(), "", "127.0.0.1", 2345)
	s.assertRsyslogUpgrade(c, rsyslogContent)

	// Lock directory
	lockdir := filepath.Join(s.DataDir(), "locks")
	c.Assert(lockdir, jc.IsDirectory)
}

func (s *UpgradeSuite) assertRsyslogUpgrade(c *gc.C, configContent string) {
	data, err := ioutil.ReadFile(s.rsyslogPath)
	c.Assert(err, gc.IsNil)
	c.Assert(string(data), gc.Equals, configContent)
}
