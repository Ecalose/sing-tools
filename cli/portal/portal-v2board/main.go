package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io/ioutil"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/sagernet/sing-tools/cli/portal"
	"github.com/sagernet/sing-tools/extensions/acme"
	_ "github.com/sagernet/sing-tools/extensions/log"
	"github.com/sagernet/sing-tools/extensions/user"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/bufio"
	E "github.com/sagernet/sing/common/exceptions"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/protocol/trojan"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

var configPath string

func main() {
	command := &cobra.Command{
		Use:  "portal-v2board [-c config.json]",
		Args: cobra.NoArgs,
		Run:  run,
	}

	command.Flags().StringVarP(&configPath, "config", "c", "config.json", "set config path")

	if err := command.Execute(); err != nil {
		logrus.Fatal(err)
	}
}

var acmeManager *acme.CertificateManager

func run(cmd *cobra.Command, args []string) {
	data, err := ioutil.ReadFile(configPath)
	if err != nil {
		logrus.Fatal(E.Cause(err, "read config"))
	}
	config := new(Config)
	err = json.Unmarshal(data, config)
	if err != nil {
		logrus.Fatal(E.Cause(err, "parse config"))
	}
	if config.Debug {
		logrus.SetLevel(logrus.TraceLevel)
	}
	if len(config.Nodes) == 0 {
		logrus.Fatal("empty nodes")
	}
	if config.ACME != nil && config.ACME.Enabled {
		err = config.ACME.SetupEnvironment()
		if err != nil {
			logrus.Fatal(err)
		}
		acmeManager = acme.NewCertificateManager(config.ACME)
	}
	var instances []Instance
	for _, node := range config.Nodes {
		client := NewNodeClient(config.URL, config.Token, strconv.Itoa(node.ID))
		switch node.Type {
		case "trojan":
			instances = append(instances, NewTrojanInstance(client, node))
		default:
			logrus.Fatal("unsupported node type ", node.Type, " (id: ", node.ID, ")")
		}
	}
	for _, instance := range instances {
		err = instance.Start()
		if err != nil {
			logrus.Fatal(err)
		}
	}

	osSignals := make(chan os.Signal, 1)
	signal.Notify(osSignals, os.Interrupt, syscall.SIGTERM)
	<-osSignals

	for _, instance := range instances {
		instance.Close()
	}
}

type Instance interface {
	Start() error
	Close() error
}

type TrojanInstance struct {
	*NodeClient
	id           int
	domain       string
	listener     net.Listener
	tlsConfig    tls.Config
	service      trojan.Service[int]
	user         *user.TrafficManager[int]
	reloadTicker *time.Ticker
}

func NewTrojanInstance(client *NodeClient, node Node) *TrojanInstance {
	t := &TrojanInstance{
		NodeClient: client,
		id:         node.ID,
		domain:     node.Domain,
		user:       user.NewTrafficManager[int](),
	}
	t.service = trojan.NewService[int](t)
	return t
}

func (i *TrojanInstance) Start() error {
	err := i.reloadUsers()
	if err != nil {
		return err
	}

	trojanConfig, err := i.GetTrojanConfig(context.Background())
	if err != nil {
		return E.Cause(err, i.id, ": read trojan config")
	}

	if trojanConfig.SNI != "" {
		i.domain = trojanConfig.SNI
	}

	if acmeManager != nil {
		certificate, err := acmeManager.GetKeyPair(i.domain)
		if err != nil {
			return E.Cause(err, i.id, ": generate certificate")
		}
		i.tlsConfig.Certificates = []tls.Certificate{*certificate}
		acmeManager.RegisterUpdateListener(i.domain, func(certificate *tls.Certificate) {
			i.tlsConfig.Certificates = []tls.Certificate{*certificate}
		})
	}

	tcpListener, err := net.ListenTCP("tcp", &net.TCPAddr{
		Port: int(trojanConfig.LocalPort),
	})
	if err != nil {
		return E.Cause(err, i.id, ": listen at tcp:", trojanConfig.LocalPort, ", check server configuration!")
	}

	if len(i.tlsConfig.Certificates) == 0 {
		i.tlsConfig.GetCertificate = func(info *tls.ClientHelloInfo) (*tls.Certificate, error) {
			return portal.GenerateCertificate(info.ServerName)
		}
	} else {
		i.tlsConfig.GetCertificate = nil
	}

	i.listener = tls.NewListener(tcpListener, &i.tlsConfig)

	logrus.Info(i.id, ": started at ", tcpListener.Addr())
	go i.loopRequests()

	i.reloadTicker = time.NewTicker(time.Minute)
	go i.loopReload()
	return nil
}

func (i *TrojanInstance) NewConnection(ctx context.Context, conn net.Conn, metadata M.Metadata) error {
	userCtx := ctx.(*trojan.Context[int])
	conn = i.user.TrackConnection(userCtx.User, conn)
	logrus.Info(i.id, ": user ", userCtx.User, " TCP ", metadata.Source, " ==> ", metadata.Destination)
	destConn, err := N.SystemDialer.DialContext(context.Background(), "tcp", metadata.Destination)
	if err != nil {
		return err
	}
	return bufio.CopyConn(ctx, conn, destConn)
}

func (i *TrojanInstance) NewPacketConnection(ctx context.Context, conn N.PacketConn, metadata M.Metadata) error {
	userCtx := ctx.(*trojan.Context[int])
	conn = i.user.TrackPacketConnection(userCtx.User, conn)
	logrus.Info(i.id, ": user ", userCtx.User, " UDP ", metadata.Source, " ==> ", metadata.Destination)
	udpConn, err := net.ListenUDP("udp", nil)
	if err != nil {
		return err
	}
	return bufio.CopyNetPacketConn(ctx, conn, udpConn)
}

func (i *TrojanInstance) loopRequests() {
	for {
		conn, err := i.listener.Accept()
		if err != nil {
			logrus.Debug(E.Cause(err, i.id, ": listener exited"))
			return
		}
		go func() {
			hErr := i.service.NewConnection(context.Background(), conn, M.Metadata{
				Protocol: "tls",
				Source:   M.SocksaddrFromNet(conn.RemoteAddr()),
			})
			if hErr != nil {
				i.HandleError(hErr)
			}
		}()
	}
}

func (i *TrojanInstance) loopReload() {
	for range i.reloadTicker.C {
		err := i.reloadUsers()
		if err != nil {
			i.HandleError(E.Cause(err, "reload user"))
		}
		traffics := i.user.ReadTraffics()
		if len(traffics) > 0 {
			var userTraffics []UserTraffic
			for userId, traffic := range traffics {
				userTraffics = append(userTraffics, UserTraffic{
					UID:      userId,
					Upload:   int64(traffic.Upload),
					Download: int64(traffic.Download),
				})
			}

			err = i.ReportTrojanTraffic(context.Background(), userTraffics)
			if err != nil {
				i.HandleError(E.Cause(err, "report traffic"))
			}
		}
	}
}

func (i *TrojanInstance) reloadUsers() error {
	logrus.Debug(i.id, ": fetching users...")
	userList, err := i.GetTrojanUserList(context.Background())
	if err != nil {
		return E.Cause(err, i.id, ": get user list")
	}
	if len(userList.Users) == 0 {
		logrus.Warn(i.id, ": empty users")
	}

	i.service.ResetUsers()
	for id, password := range userList.Users {
		err = i.service.AddUser(id, password)
		if err != nil {
			logrus.Warn(E.Cause(err, i.id, ": add user"))
		}
	}

	logrus.Debug(i.id, ": loaded ", len(userList.Users), " users")
	return nil
}

func (i *TrojanInstance) HandleError(err error) {
	common.Close(err)
	if E.IsClosed(err) {
		return
	}
	logrus.Warn(i.id, ": ", err)
}

func (i *TrojanInstance) Close() error {
	i.reloadTicker.Stop()
	return i.listener.Close()
}
