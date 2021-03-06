package main

import (
	"net/http"
	"os/exec"
	"time"

	"github.com/go-check/check"

	checker "github.com/vdemeester/shakers"
)

// Etcd test suites (using libcompose)
type EtcdSuite struct{ BaseSuite }

func (s *EtcdSuite) SetUpSuite(c *check.C) {
	s.createComposeProject(c, "etcd")
}

func (s *EtcdSuite) TestSimpleConfiguration(c *check.C) {
	cmd := exec.Command(traefikBinary, "--configFile=fixtures/etcd/simple.toml")
	err := cmd.Start()
	c.Assert(err, checker.IsNil)
	defer cmd.Process.Kill()

	time.Sleep(1000 * time.Millisecond)
	// TODO validate : run on 80
	resp, err := http.Get("http://127.0.0.1:8000/")

	// Expected a 404 as we did not configure anything
	c.Assert(err, checker.IsNil)
	c.Assert(resp.StatusCode, checker.Equals, 404)
}
