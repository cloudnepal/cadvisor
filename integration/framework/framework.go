// Copyright 2014 Google Inc. All Rights Reserved.
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

package framework

import (
	"bytes"
	"flag"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/cadvisor/client"
	"github.com/google/cadvisor/integration/common"
)

var host = flag.String("host", "localhost", "Address of the host being tested")
var port = flag.Int("port", 8080, "Port of the application on the host being tested")

// Integration test framework.
type Framework interface {
	// Clean the framework state.
	Cleanup()

	// The testing.T used by the framework and the current test.
	T() *testing.T

	// Returns the hostname being tested.
	Hostname() HostnameInfo

	// Returns the Docker actions for the test framework.
	Docker() DockerActions

	// Returns the shell actions for the test framework.
	Shell() ShellActions

	// Returns the cAdvisor actions for the test framework.
	Cadvisor() CadvisorActions
}

// Instantiates a Framework. Cleanup *must* be called. Class is thread-compatible.
// All framework actions report fatal errors on the t specified at creation time.
//
// Typical use:
//
// func TestFoo(t *testing.T) {
// 	fm := framework.New(t)
// 	defer fm.Cleanup()
//      ... actual test ...
// }
func New(t *testing.T) Framework {
	// All integration tests are large.
	if testing.Short() {
		t.Skip("Skipping framework test in short mode")
	}

	// Try to see if non-localhost hosts are GCE instances.
	var gceInstanceName string
	hostname := *host
	if hostname != "localhost" {
		gceInstanceName = hostname
		gceIp, err := common.GetGceIp(hostname)
		if err == nil {
			hostname = gceIp
		}
	}

	fm := &realFramework{
		hostname: HostnameInfo{
			Host:            hostname,
			Port:            *port,
			GceInstanceName: gceInstanceName,
		},
		t:        t,
		cleanups: make([]func(), 0),
	}
	fm.shellActions = shellActions{
		fm: fm,
	}
	fm.dockerActions = dockerActions{
		fm: fm,
	}

	return fm
}

type DockerActions interface {
	// Run the no-op pause Docker container and return its ID.
	RunPause() string

	// Run the specified command in a Docker busybox container and return its ID.
	RunBusybox(cmd ...string) string

	// Runs a Docker container in the background. Uses the specified DockerRunArgs and command.
	// Returns the ID of the new container.
	//
	// e.g.:
	// Run(DockerRunArgs{Image: "busybox"}, "ping", "www.google.com")
	//   -> docker run busybox ping www.google.com
	Run(args DockerRunArgs, cmd ...string) string
}

type ShellActions interface {
	// Runs a specified command and arguments. Returns the stdout and stderr.
	Run(cmd string, args ...string) (string, string)
}

type CadvisorActions interface {
	// Returns a cAdvisor client to the machine being tested.
	Client() *client.Client
}

type realFramework struct {
	hostname       HostnameInfo
	t              *testing.T
	cadvisorClient *client.Client

	shellActions  shellActions
	dockerActions dockerActions

	// Cleanup functions to call on Cleanup()
	cleanups []func()
}

type shellActions struct {
	fm *realFramework
}

type dockerActions struct {
	fm *realFramework
}

type HostnameInfo struct {
	Host            string
	Port            int
	GceInstanceName string
}

// Returns: http://<host>:<port>/
func (self HostnameInfo) FullHostname() string {
	return fmt.Sprintf("http://%s:%d/", self.Host, self.Port)
}

func (self *realFramework) T() *testing.T {
	return self.t
}

func (self *realFramework) Hostname() HostnameInfo {
	return self.hostname
}

func (self *realFramework) Shell() ShellActions {
	return self.shellActions
}

func (self *realFramework) Docker() DockerActions {
	return self.dockerActions
}

func (self *realFramework) Cadvisor() CadvisorActions {
	return self
}

// Call all cleanup functions.
func (self *realFramework) Cleanup() {
	for _, cleanupFunc := range self.cleanups {
		cleanupFunc()
	}
}

// Gets a client to the cAdvisor being tested.
func (self *realFramework) Client() *client.Client {
	if self.cadvisorClient == nil {
		cadvisorClient, err := client.NewClient(self.Hostname().FullHostname())
		if err != nil {
			self.t.Fatalf("Failed to instantiate the cAdvisor client: %v", err)
		}
		self.cadvisorClient = cadvisorClient
	}
	return self.cadvisorClient
}

func (self dockerActions) RunPause() string {
	return self.Run(DockerRunArgs{
		Image: "kubernetes/pause",
	})
}

// Run the specified command in a Docker busybox container.
func (self dockerActions) RunBusybox(cmd ...string) string {
	return self.Run(DockerRunArgs{
		Image: "busybox",
	}, cmd...)
}

type DockerRunArgs struct {
	// Image to use.
	Image string

	// Arguments to the Docker CLI.
	Args []string
}

// TODO(vmarmol): Use the Docker remote API.
// TODO(vmarmol): Refactor a set of "RunCommand" actions.
// Runs a Docker container in the background. Uses the specified DockerRunArgs and command.
//
// e.g.:
// RunDockerContainer(DockerRunArgs{Image: "busybox"}, "ping", "www.google.com")
//   -> docker run busybox ping www.google.com
func (self dockerActions) Run(args DockerRunArgs, cmd ...string) string {
	dockerCommand := append(append(append([]string{"docker", "run", "-d"}, args.Args...), args.Image), cmd...)

	output, _ := self.fm.Shell().Run("sudo", dockerCommand...)

	// The last line is the container ID.
	elements := strings.Fields(output)
	containerId := elements[len(elements)-1]

	self.fm.cleanups = append(self.fm.cleanups, func() {
		self.fm.Shell().Run("sudo", "docker", "rm", "-f", containerId)
	})
	return containerId
}

func (self shellActions) Run(command string, args ...string) (string, string) {
	var cmd *exec.Cmd
	if self.fm.Hostname().Host == "localhost" {
		// Just run locally.
		cmd = exec.Command(command, args...)
	} else {
		// We must SSH to the remote machine and run the command.
		cmd = exec.Command("gcutil", append([]string{"ssh", self.fm.Hostname().GceInstanceName, command}, args...)...)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		self.fm.T().Fatalf("Failed to run %q %v in %q with error: %q. Stdout: %q, Stderr: %s", command, args, self.fm.Hostname().Host, err, stdout.String(), stderr.String())
		return "", ""
	}
	return stdout.String(), stderr.String()
}

// Runs retryFunc until no error is returned. After dur time the last error is returned.
// Note that the function does not timeout the execution of retryFunc when the limit is reached.
func RetryForDuration(retryFunc func() error, dur time.Duration) error {
	waitUntil := time.Now().Add(dur)
	var err error
	for time.Now().Before(waitUntil) {
		err = retryFunc()
		if err == nil {
			return nil
		}
	}
	return err
}
