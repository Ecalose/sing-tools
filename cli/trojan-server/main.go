package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io/ioutil"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"syscall"

	"github.com/sagernet/sing-tools/cli/portal"
	"github.com/sagernet/sing-tools/extensions/acme"
	_ "github.com/sagernet/sing-tools/extensions/log"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/bufio"
	E "github.com/sagernet/sing/common/exceptions"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/protocol/trojan"
	"github.com/sagernet/sing/transport/tcp"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

const udpTimeout = 5 * 60

type flags struct {
	Server     string         `json:"server"`
	ServerPort uint16         `json:"server_port"`
	ServerName string         `json:"server_name"`
	Bind       string         `json:"local_address"`
	LocalPort  uint16         `json:"local_port"`
	Password   string         `json:"password"`
	Verbose    bool           `json:"verbose"`
	Insecure   bool           `json:"insecure"`
	ACME       *acme.Settings `json:"acme"`
	ConfigFile string
}

func main() {
	f := new(flags)

	command := &cobra.Command{
		Use:   "trojan-local",
		Short: "trojan client",
		Run: func(cmd *cobra.Command, args []string) {
			run(cmd, f)
		},
	}

	command.Flags().StringVarP(&f.Server, "server", "s", "", "Store the server’s hostname or IP.")
	command.Flags().Uint16VarP(&f.ServerPort, "server-port", "p", 0, "Store the server’s port number.")
	command.Flags().StringVarP(&f.Bind, "local-address", "b", "", "Store the local address.")
	command.Flags().Uint16VarP(&f.LocalPort, "local-port", "l", 0, "Store the local port number.")
	command.Flags().StringVarP(&f.Password, "password", "k", "", "Store the password. The server and the client should use the same password.")
	command.Flags().BoolVarP(&f.Insecure, "insecure", "i", false, "Store insecure.")
	command.Flags().StringVarP(&f.ConfigFile, "config", "c", "", "Use a configuration file.")
	command.Flags().BoolVarP(&f.Verbose, "verbose", "v", false, "Store verbose mode.")

	err := command.Execute()
	if err != nil {
		logrus.Fatal(err)
	}
}

func run(cmd *cobra.Command, f *flags) {
	c, err := newServer(f)
	if err != nil {
		logrus.StandardLogger().Log(logrus.FatalLevel, err, "\n\n")
		cmd.Help()
		os.Exit(1)
	}

	if f.ACME != nil && f.ACME.Enabled {
		err = f.ACME.SetupEnvironment()
		if err != nil {
			logrus.Fatal(err)
		}
		acmeManager := acme.NewCertificateManager(f.ACME)
		certificate, err := acmeManager.GetKeyPair(f.ServerName)
		if err != nil {
			logrus.Fatal(err)
		}
		c.tlsConfig.Certificates = []tls.Certificate{*certificate}
		acmeManager.RegisterUpdateListener(f.ServerName, func(certificate *tls.Certificate) {
			c.tlsConfig.Certificates = []tls.Certificate{*certificate}
		})
	}

	err = c.tcpIn.Start()
	if err != nil {
		logrus.Fatal(err)
	}

	logrus.Info("server started at ", c.tcpIn.Addr())

	osSignals := make(chan os.Signal, 1)
	signal.Notify(osSignals, os.Interrupt, syscall.SIGTERM)
	<-osSignals

	c.tcpIn.Close()
}

type server struct {
	tcpIn     *tcp.Listener
	service   trojan.Service[int]
	tlsConfig tls.Config
}

func newServer(f *flags) (*server, error) {
	s := new(server)

	if f.ConfigFile != "" {
		configFile, err := ioutil.ReadFile(f.ConfigFile)
		if err != nil {
			return nil, E.Cause(err, "read config file")
		}
		flagsNew := new(flags)
		err = json.Unmarshal(configFile, flagsNew)
		if err != nil {
			return nil, E.Cause(err, "decode config file")
		}
		if flagsNew.Server != "" && f.Server == "" {
			f.Server = flagsNew.Server
		}
		if flagsNew.ServerPort != 0 && f.ServerPort == 0 {
			f.ServerPort = flagsNew.ServerPort
		}
		if flagsNew.ServerName != "" && f.ServerName == "" {
			f.ServerName = flagsNew.ServerName
		}
		if flagsNew.Bind != "" && f.Bind == "" {
			f.Bind = flagsNew.Bind
		}
		if flagsNew.LocalPort != 0 && f.LocalPort == 0 {
			f.LocalPort = flagsNew.LocalPort
		}
		if flagsNew.Password != "" && f.Password == "" {
			f.Password = flagsNew.Password
		}
		if flagsNew.Insecure {
			f.Insecure = true
		}
		if flagsNew.ACME != nil {
			f.ACME = flagsNew.ACME
		}
		if flagsNew.Verbose {
			f.Verbose = true
		}
	}

	if f.Verbose {
		logrus.SetLevel(logrus.TraceLevel)
	}

	if f.Server == "" {
		return nil, E.New("missing server address")
	} else if f.ServerPort == 0 {
		return nil, E.New("missing server port")
	}

	var bind netip.Addr
	if f.Server != "" {
		addr, err := netip.ParseAddr(f.Server)
		if err != nil {
			return nil, E.Cause(err, "bad server address")
		}
		bind = addr
	} else {
		bind = netip.IPv6Unspecified()
	}
	s.service = trojan.NewService[int](s)
	common.Must(s.service.AddUser(0, f.Password))
	s.tcpIn = tcp.NewTCPListener(netip.AddrPortFrom(bind, f.ServerPort), s)
	return s, nil
}

func (s *server) NewConnection(ctx context.Context, conn net.Conn, metadata M.Metadata) error {
	if metadata.Protocol != "trojan" {
		logrus.Trace("inbound raw TCP from ", metadata.Source)
		if len(s.tlsConfig.Certificates) == 0 {
			s.tlsConfig.GetCertificate = func(info *tls.ClientHelloInfo) (*tls.Certificate, error) {
				return portal.GenerateCertificate(info.ServerName)
			}
		} else {
			s.tlsConfig.GetCertificate = nil
		}
		return s.service.NewConnection(ctx, tls.Server(conn, &s.tlsConfig), metadata)
	}
	destConn, err := N.SystemDialer.DialContext(context.Background(), "tcp", metadata.Destination)
	if err != nil {
		return err
	}
	logrus.Info("inbound TCP ", conn.RemoteAddr(), " ==> ", metadata.Destination)
	return bufio.CopyConn(ctx, conn, destConn)
}

func (s *server) NewPacketConnection(ctx context.Context, conn N.PacketConn, metadata M.Metadata) error {
	logrus.Info("inbound UDP ", metadata.Source, " ==> ", metadata.Destination)
	udpConn, err := net.ListenUDP("udp", nil)
	if err != nil {
		return err
	}
	return bufio.CopyPacketConn(ctx, conn, bufio.NewPacketConn(udpConn))
}

func (s *server) HandleError(err error) {
	common.Close(err)
	if E.IsClosed(err) {
		return
	}
	logrus.Warn(err)
}
