/*
Copyright 2015 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package client

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/gravitational/teleport/lib/auth"
	authority "github.com/gravitational/teleport/lib/auth/testauthority"
	"github.com/gravitational/teleport/lib/backend/boltbk"
	"github.com/gravitational/teleport/lib/backend/encryptedbk"
	"github.com/gravitational/teleport/lib/backend/encryptedbk/encryptor"
	"github.com/gravitational/teleport/lib/events/boltlog"
	"github.com/gravitational/teleport/lib/limiter"
	"github.com/gravitational/teleport/lib/recorder/boltrec"
	"github.com/gravitational/teleport/lib/reversetunnel"
	"github.com/gravitational/teleport/lib/services"
	sess "github.com/gravitational/teleport/lib/session"
	"github.com/gravitational/teleport/lib/srv"
	"github.com/gravitational/teleport/lib/sshutils"
	"github.com/gravitational/teleport/lib/teleagent"
	"github.com/gravitational/teleport/lib/utils"
	"github.com/gravitational/teleport/lib/web"

	"github.com/gravitational/teleport/Godeps/_workspace/src/github.com/gokyle/hotp"
	"github.com/gravitational/teleport/Godeps/_workspace/src/github.com/mailgun/lemma/secret"
	"github.com/gravitational/teleport/Godeps/_workspace/src/golang.org/x/crypto/ssh"
	. "github.com/gravitational/teleport/Godeps/_workspace/src/gopkg.in/check.v1"

	"github.com/gravitational/teleport/Godeps/_workspace/src/github.com/gravitational/log"
)

func TestClient(t *testing.T) { TestingT(t) }

type ClientSuite struct {
	srv          *srv.Server
	srv2         *srv.Server
	proxy        *srv.Server
	srvAddress   string
	srv2Address  string
	proxyAddress string
	webAddress   string
	clt          *ssh.Client
	bk           *encryptedbk.ReplicatedBackend
	a            *auth.AuthServer
	scrt         secret.SecretService
	signer       ssh.Signer
	teleagent    *teleagent.TeleAgent
	dir          string
	dir2         string
}

var _ = Suite(&ClientSuite{})

func (s *ClientSuite) SetUpSuite(c *C) {
	key, err := secret.NewKey()
	c.Assert(err, IsNil)
	scrt, err := secret.New(&secret.Config{KeyBytes: key})
	c.Assert(err, IsNil)
	s.scrt = scrt

	s.dir = c.MkDir()
	s.dir2 = c.MkDir()

	allowAllLimiter, err := limiter.NewLimiter(limiter.LimiterConfig{})

	baseBk, err := boltbk.New(filepath.Join(s.dir, "db"))
	c.Assert(err, IsNil)
	s.bk, err = encryptedbk.NewReplicatedBackend(baseBk,
		filepath.Join(s.dir, "keys"), nil,
		encryptor.GetTestKey)
	c.Assert(err, IsNil)

	s.a = auth.NewAuthServer(s.bk, authority.New(), s.scrt, "host5")

	// set up host private key and certificate
	c.Assert(s.a.ResetHostCertificateAuthority(""), IsNil)
	hpriv, hpub, err := s.a.GenerateKeyPair("")
	c.Assert(err, IsNil)
	hcert, err := s.a.GenerateHostCert(hpub, "localhost", "localhost", auth.RoleAdmin, 0)
	c.Assert(err, IsNil)

	// set up user CA and set up a user that has access to the server
	c.Assert(s.a.ResetUserCertificateAuthority(""), IsNil)

	s.signer, err = sshutils.NewSigner(hpriv, hcert)
	c.Assert(err, IsNil)

	ap := auth.NewBackendAccessPoint(s.bk)

	// Starting node1
	s.srvAddress = "127.0.0.1:30185"
	s.srv, err = srv.New(
		utils.NetAddr{Network: "tcp", Addr: s.srvAddress},
		"localhost",
		[]ssh.Signer{s.signer},
		ap,
		allowAllLimiter,
		s.dir,
		srv.SetShell("/bin/sh"),
		srv.SetLabels(
			map[string]string{"label1": "value1", "label2": "value2"},
			services.CommandLabels{
				"cmdLabel1": services.CommandLabel{
					Period:  time.Second,
					Command: []string{"expr", "1", "+", "3"}},
			},
		),
	)
	c.Assert(err, IsNil)
	c.Assert(s.srv.Start(), IsNil)

	// Starting node2
	s.srv2Address = "127.0.0.1:30189"
	s.srv2, err = srv.New(
		utils.NetAddr{Network: "tcp", Addr: s.srv2Address},
		"localhost",
		[]ssh.Signer{s.signer},
		ap,
		allowAllLimiter,
		s.dir2,
		srv.SetShell("/bin/sh"),
		srv.SetLabels(
			map[string]string{"label1": "value1"},
			services.CommandLabels{
				"cmdLabel1": services.CommandLabel{
					Period:  time.Second,
					Command: []string{"expr", "1", "+", "4"},
				},
				"cmdLabel2": services.CommandLabel{
					Period:  time.Second,
					Command: []string{"expr", "1", "+", "5"},
				},
			},
		),
	)
	c.Assert(err, IsNil)
	c.Assert(s.srv2.Start(), IsNil)

	// Starting proxy
	reverseTunnelAddress := utils.NetAddr{Network: "tcp", Addr: "localhost:33056"}
	reverseTunnelServer, err := reversetunnel.NewServer(
		reverseTunnelAddress,
		[]ssh.Signer{s.signer},
		ap, allowAllLimiter)
	c.Assert(err, IsNil)
	c.Assert(reverseTunnelServer.Start(), IsNil)

	s.proxyAddress = "localhost:34783"

	s.proxy, err = srv.New(
		utils.NetAddr{Network: "tcp", Addr: s.proxyAddress},
		"localhost",
		[]ssh.Signer{s.signer},
		ap,
		allowAllLimiter,
		s.dir,
		srv.SetProxyMode(reverseTunnelServer),
	)
	c.Assert(err, IsNil)
	c.Assert(s.proxy.Start(), IsNil)

	bl, err := boltlog.New(filepath.Join(s.dir, "eventsdb"))
	c.Assert(err, IsNil)

	rec, err := boltrec.New(s.dir)
	c.Assert(err, IsNil)

	apiSrv := auth.NewAPIWithRoles(s.a, bl, sess.New(s.bk), rec,
		auth.NewAllowAllPermissions(),
		auth.StandardRoles,
	)
	apiSrv.Serve()

	tsrv, err := auth.NewTunServer(
		utils.NetAddr{Network: "tcp", Addr: "localhost:31497"},
		[]ssh.Signer{s.signer},
		apiSrv, s.a, allowAllLimiter)
	c.Assert(err, IsNil)
	c.Assert(tsrv.Start(), IsNil)

	user := "user1"
	pass := []byte("utndkrn")

	hotpURL, _, err := s.a.UpsertPassword(user, pass)
	c.Assert(err, IsNil)
	otp, _, err := hotp.FromURL(hotpURL)
	c.Assert(err, IsNil)
	otp.Increment()

	authMethod, err := auth.NewWebPasswordAuth(user, pass, otp.OTP())
	c.Assert(err, IsNil)

	tunClt, err := auth.NewTunClient(
		utils.NetAddr{Network: "tcp", Addr: tsrv.Addr()}, user, authMethod)
	c.Assert(err, IsNil)

	rsAgent, err := reversetunnel.NewAgent(
		reverseTunnelAddress,
		"localhost",
		[]ssh.Signer{s.signer}, tunClt)
	c.Assert(err, IsNil)
	c.Assert(rsAgent.Start(), IsNil)

	webHandler, err := web.NewMultiSiteHandler(
		web.MultiSiteConfig{
			Tun:        reverseTunnelServer,
			AssetsDir:  "../../assets/web",
			AuthAddr:   utils.NetAddr{Network: "tcp", Addr: tsrv.Addr()},
			DomainName: "localhost",
		},
	)
	c.Assert(err, IsNil)

	s.webAddress = "localhost:31386"

	go func() {
		err := http.ListenAndServe(s.webAddress, webHandler)
		if err != nil {
			log.Errorf(err.Error())
		}
	}()

	s.teleagent = teleagent.NewTeleAgent()
	err = s.teleagent.Login("http://"+s.webAddress, user, string(pass), otp.OTP(), time.Minute)
	c.Assert(err, IsNil)

	// "Command labels will be calculated only on the second heartbeat"
	time.Sleep(time.Millisecond * 3100)
}

func (s *ClientSuite) TestRunCommand(c *C) {
	nodeClient, err := ConnectToNode(s.srvAddress,
		s.teleagent.AuthMethod(), "user1")
	c.Assert(err, IsNil)

	out, err := nodeClient.Run("expr 3 + 5")
	c.Assert(err, IsNil)
	c.Assert(out, Equals, "8\n")
}

func (s *ClientSuite) TestConnectViaProxy(c *C) {
	proxyClient, err := ConnectToProxy(s.proxyAddress,
		s.teleagent.AuthMethod(), "user1")
	c.Assert(err, IsNil)

	nodeClient, err := proxyClient.ConnectToNode(s.srvAddress,
		s.teleagent.AuthMethod(), "user1")
	c.Assert(err, IsNil)

	out, err := nodeClient.Run("expr 3 + 5")
	c.Assert(err, IsNil)
	c.Assert(out, Equals, "8\n")
}

func (s *ClientSuite) TestShell(c *C) {
	proxyClient, err := ConnectToProxy(s.proxyAddress,
		s.teleagent.AuthMethod(), "user1")
	c.Assert(err, IsNil)

	nodeClient, err := proxyClient.ConnectToNode(s.srvAddress,
		s.teleagent.AuthMethod(), "user1")
	c.Assert(err, IsNil)

	shell, err := nodeClient.Shell()
	c.Assert(err, IsNil)

	// run first command
	_, err = shell.Write([]byte("expr 11 + 22\n"))
	c.Assert(err, IsNil)
	time.Sleep(time.Millisecond * 100)

	out := make([]byte, 100)
	n, err := shell.Read(out)
	c.Assert(err, IsNil)
	c.Assert(string(out[:n]), Equals, "$ expr 11 + 22\r\n33\r\n$ ")

	// run second command
	_, err = shell.Write([]byte("expr 2 + 3\n"))
	c.Assert(err, IsNil)
	time.Sleep(time.Millisecond * 100)

	n, err = shell.Read(out)
	c.Assert(err, IsNil)
	c.Assert(string(out[:n]), Equals, "expr 2 + 3\r\n5\r\n$ ")

	c.Assert(shell.Close(), IsNil)
}

func (s *ClientSuite) TestGetServer(c *C) {
	proxyClient, err := ConnectToProxy(s.proxyAddress,
		s.teleagent.AuthMethod(), "user1")
	c.Assert(err, IsNil)

	server1Info := services.Server{
		ID:       "127.0.0.1_30185",
		Addr:     s.srvAddress,
		Hostname: "localhost",
		Labels: map[string]string{
			"label1": "value1",
			"label2": "value2",
		},
		CmdLabels: map[string]services.CommandLabel{
			"cmdLabel1": services.CommandLabel{
				Period:  time.Second,
				Command: []string{"expr", "1", "+", "3"},
				Result:  "4\n",
			},
		},
	}

	server2Info := services.Server{
		ID:       "127.0.0.1_30189",
		Addr:     s.srv2Address,
		Hostname: "localhost",
		Labels: map[string]string{
			"label1": "value1",
		},
		CmdLabels: map[string]services.CommandLabel{
			"cmdLabel1": services.CommandLabel{
				Period:  time.Second,
				Command: []string{"expr", "1", "+", "4"},
				Result:  "5\n",
			},
			"cmdLabel2": services.CommandLabel{
				Period:  time.Second,
				Command: []string{"expr", "1", "+", "5"},
				Result:  "6\n",
			},
		},
	}

	servers, err := proxyClient.GetServers()
	c.Assert(err, IsNil)
	c.Assert(servers, DeepEquals, []services.Server{
		server1Info,
		server2Info,
	})

	servers, err = proxyClient.FindServers("label1", "value1")
	c.Assert(err, IsNil)
	c.Assert(servers, DeepEquals, []services.Server{
		server1Info,
		server2Info,
	})

	servers, err = proxyClient.FindServers("label2", "value2")
	c.Assert(err, IsNil)
	c.Assert(servers, DeepEquals, []services.Server{
		server1Info,
	})

	servers, err = proxyClient.FindServers("cmdLabel1", "4")
	c.Assert(err, IsNil)
	c.Assert(servers, DeepEquals, []services.Server{
		server1Info,
	})

	servers, err = proxyClient.FindServers("cmdLabel1", "5")
	c.Assert(err, IsNil)
	c.Assert(servers, DeepEquals, []services.Server{
		server2Info,
	})

	servers, err = proxyClient.FindServers("cmdLabel2", "6")
	c.Assert(err, IsNil)
	c.Assert(servers, DeepEquals, []services.Server{
		server2Info,
	})

}
