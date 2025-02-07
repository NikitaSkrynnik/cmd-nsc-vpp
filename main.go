// Copyright (c) 2021-2022 Doc.ai its affiliates.
//
// SPDX-License-Identifier: Apache-2.0
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

//go:build linux
// +build linux

package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	nested "github.com/antonfisher/nested-logrus-formatter"
	"github.com/edwarnicke/debug"
	"github.com/edwarnicke/grpcfd"
	"github.com/edwarnicke/vpphelper"
	"github.com/golang/protobuf/ptypes/empty"
	"github.com/kelseyhightower/envconfig"
	"github.com/networkservicemesh/sdk/pkg/networkservice/core/next"
	"github.com/sirupsen/logrus"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/networkservicemesh/api/pkg/api/networkservice"
	"github.com/networkservicemesh/govpp/binapi/interface_types"
	"github.com/networkservicemesh/govpp/binapi/ip_types"
	"github.com/networkservicemesh/govpp/binapi/ping"
	"github.com/networkservicemesh/sdk-vpp/pkg/networkservice/connectioncontext"
	"github.com/networkservicemesh/sdk-vpp/pkg/networkservice/mechanisms/memif"
	"github.com/networkservicemesh/sdk-vpp/pkg/networkservice/up"
	"github.com/networkservicemesh/sdk-vpp/pkg/tools/ifindex"

	"github.com/networkservicemesh/sdk/pkg/networkservice/chains/client"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/clientinfo"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/excludedprefixes"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/heal"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/mechanisms/recvfd"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/mechanisms/sendfd"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/retry"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/upstreamrefresh"
	"github.com/networkservicemesh/sdk/pkg/tools/awarenessgroups"
	"github.com/networkservicemesh/sdk/pkg/tools/grpcutils"
	"github.com/networkservicemesh/sdk/pkg/tools/log"
	"github.com/networkservicemesh/sdk/pkg/tools/log/logruslogger"
	"github.com/networkservicemesh/sdk/pkg/tools/nsurl"
	"github.com/networkservicemesh/sdk/pkg/tools/opentelemetry"
	"github.com/networkservicemesh/sdk/pkg/tools/spiffejwt"
	"github.com/networkservicemesh/sdk/pkg/tools/token"
	"github.com/networkservicemesh/sdk/pkg/tools/tracing"
)

// Config - configuration for cmd-forwarder-vpp
type Config struct {
	Name                  string                  `default:"cmd-nsc-vpp" desc:"Name of Endpoint"`
	DialTimeout           time.Duration           `default:"5s" desc:"timeout to dial NSMgr" split_words:"true"`
	RequestTimeout        time.Duration           `default:"35s" desc:"timeout to request NSE" split_words:"true"`
	ConnectTo             url.URL                 `default:"unix:///var/lib/networkservicemesh/nsm.io.sock" desc:"url to connect to" split_words:"true"`
	MaxTokenLifetime      time.Duration           `default:"10m" desc:"maximum lifetime of tokens" split_words:"true"`
	NetworkServices       []url.URL               `default:"" desc:"A list of Network Service Requests" split_words:"true"`
	AwarenessGroups       awarenessgroups.Decoder `defailt:"" desc:"Awareness groups for mutually aware NSEs" split_words:"true"`
	LogLevel              string                  `default:"INFO" desc:"Log level" split_words:"true"`
	OpenTelemetryEndpoint string                  `default:"otel-collector.observability.svc.cluster.local:4317" desc:"OpenTelemetry Collector Endpoint"`
}

type ifIndexGetClient struct {
	ifindex *interface_types.InterfaceIndex
}

func NewClient(ctx context.Context, ifindex *interface_types.InterfaceIndex) networkservice.NetworkServiceClient {
	return &ifIndexGetClient{
		ifindex: ifindex,
	}
}

func (u *ifIndexGetClient) Request(ctx context.Context, request *networkservice.NetworkServiceRequest, opts ...grpc.CallOption) (*networkservice.Connection, error) {
	conn, err := next.Client(ctx).Request(ctx, request, opts...)

	ifindex, _ := ifindex.Load(ctx, true)
	*u.ifindex = ifindex
	log.FromContext(ctx).Infof("ifindex: %v", ifindex)

	return conn, err
}

func (u *ifIndexGetClient) Close(ctx context.Context, conn *networkservice.Connection, opts ...grpc.CallOption) (*empty.Empty, error) {
	return next.Client(ctx).Close(ctx, conn, opts...)
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ********************************************************************************
	// setup logging
	// ********************************************************************************
	logrus.SetFormatter(&nested.Formatter{})
	log.EnableTracing(true)
	ctx = log.WithLog(ctx, logruslogger.New(ctx, map[string]interface{}{"cmd": os.Args[0]}))

	// ********************************************************************************
	// Debug self if necessary
	// ********************************************************************************
	if err := debug.Self(); err != nil {
		log.FromContext(ctx).Infof("%s", err)
	}

	starttime := time.Now()

	// enumerating phases
	log.FromContext(ctx).Infof("there are 5 phases which will be executed followed by a success message:")
	log.FromContext(ctx).Infof("the phases include:")
	log.FromContext(ctx).Infof("1: get config from environment")
	log.FromContext(ctx).Infof("2: run vpp and get a connection to it")
	log.FromContext(ctx).Infof("3: retrieve spiffe svid")
	log.FromContext(ctx).Infof("4: create network service client")
	log.FromContext(ctx).Infof("5: connect to all passed services")
	log.FromContext(ctx).Infof("a final success message with start time duration")

	// ********************************************************************************
	log.FromContext(ctx).Infof("executing phase 1: get config from environment (time since start: %s)", time.Since(starttime))
	// ********************************************************************************
	now := time.Now()

	config := &Config{}
	if err := envconfig.Usage("nsm", config); err != nil {
		logrus.Fatal(err)
	}
	if err := envconfig.Process("nsm", config); err != nil {
		logrus.Fatalf("error processing config from env: %+v", err)
	}
	log.FromContext(ctx).Infof("Config: %#v", config)

	l, err := logrus.ParseLevel(config.LogLevel)
	if err != nil {
		logrus.Fatalf("invalid log level %s", config.LogLevel)
	}
	logrus.SetLevel(l)

	log.FromContext(ctx).WithField("duration", time.Since(now)).Infof("completed phase 1: get config from environment")

	// ********************************************************************************
	// Configure Open Telemetry
	// ********************************************************************************
	if opentelemetry.IsEnabled() {
		collectorAddress := config.OpenTelemetryEndpoint
		spanExporter := opentelemetry.InitSpanExporter(ctx, collectorAddress)
		metricExporter := opentelemetry.InitMetricExporter(ctx, collectorAddress)
		o := opentelemetry.Init(ctx, spanExporter, metricExporter, config.Name)
		defer func() {
			if err = o.Close(); err != nil {
				log.FromContext(ctx).Error(err.Error())
			}
		}()
	}

	// ********************************************************************************
	log.FromContext(ctx).Infof("executing phase 2: run vpp and get a connection to it (time since start: %s)", time.Since(starttime))
	// ********************************************************************************
	now = time.Now()

	vppConn, vppErrCh := vpphelper.StartAndDialContext(ctx)
	exitOnErrCh(ctx, cancel, vppErrCh)

	defer func() {
		cancel()
		<-vppErrCh
	}()

	log.FromContext(ctx).WithField("duration", time.Since(now)).Info("completed phase 2: run vpp and get a connection to it")

	// ********************************************************************************
	log.FromContext(ctx).Infof("executing phase 3: retrieving svid, check spire agent logs if this is the last line you see (time since start: %s)", time.Since(starttime))
	// ********************************************************************************
	now = time.Now()

	source, err := workloadapi.NewX509Source(ctx)
	if err != nil {
		logrus.Fatalf("error getting x509 source: %+v", err)
	}
	svid, err := source.GetX509SVID()
	if err != nil {
		logrus.Fatalf("error getting x509 svid: %+v", err)
	}
	logrus.Infof("SVID: %q", svid.ID)

	log.FromContext(ctx).WithField("duration", time.Since(now)).Info("completed phase 3: retrieving svid")

	tlsClientConfig := tlsconfig.MTLSClientConfig(source, source, tlsconfig.AuthorizeAny())
	tlsClientConfig.MinVersion = tls.VersionTLS12

	// ********************************************************************************
	log.FromContext(ctx).Infof("executing phase 4: create network service client (time since start: %s)", time.Since(starttime))
	// ********************************************************************************
	dialOptions := append(tracing.WithTracingDial(),
		grpc.WithDefaultCallOptions(
			grpc.PerRPCCredentials(token.NewPerRPCCredentials(spiffejwt.TokenGeneratorFunc(source, config.MaxTokenLifetime))),
		),
		grpc.WithTransportCredentials(
			grpcfd.TransportCredentials(
				credentials.NewTLS(tlsClientConfig))),
		grpcfd.WithChainStreamInterceptor(),
		grpcfd.WithChainUnaryInterceptor(),
	)

	var ifindex interface_types.InterfaceIndex

	nsmClient := client.NewClient(
		ctx,
		client.WithClientURL(&config.ConnectTo),
		client.WithName(config.Name),
		client.WithHealClient(heal.NewClient(ctx,
			heal.WithLivenessCheck(func(deadlineCtx context.Context, conn *networkservice.Connection) bool {
				l := log.FromContext(ctx)

				defer l.Info("Finish pinging")
				defaultTimeout := time.Second
				deadline, ok := deadlineCtx.Deadline()
				if !ok {
					deadline = time.Now().Add(defaultTimeout)
				}
				timeout := time.Until(deadline)

				packetCount := 4
				interval := timeout.Seconds() / float64(packetCount) * 0.7
				dstIP := conn.Context.IpContext.DstIpAddrs[0]

				var msg ping.Ping

				dstAddrStr := strings.Split(dstIP, "/")[0]

				dstAddress, _ := ip_types.ParseAddress(dstAddrStr)

				l.Infof("DstAddrStr: %v", dstAddrStr)
				l.Infof("DstAddr parsed: %v", dstAddress)

				msg.Address = dstAddress
				msg.Timeout = interval

				replyCount := 0

				for i := 0; i < packetCount; i++ {
					reply, _ := ping.NewServiceClient(vppConn).Ping(deadlineCtx, &msg)
					if deadlineCtx.Err() != nil {
						l.Info("deadline exceeded")

						if reply != nil {
							replyCount += int(reply.ReplyCount)

							l.Infof("reply.Retval: %v", reply.Retval)
							l.Infof("reply.ReplyCount: %v", reply.ReplyCount)
						}

						return replyCount > 0
					}

					if reply != nil {
						l.Infof("reply.Retval: %v", reply.Retval)
						l.Infof("reply.ReplyCount: %v", reply.ReplyCount)
					}

					replyCount += int(reply.ReplyCount)
				}

				return replyCount > 0
			}),
			heal.WithLivenessCheckInterval(time.Second*3),
			heal.WithLivenessCheckTimeout(time.Second*10))),
		client.WithAdditionalFunctionality(
			clientinfo.NewClient(),
			upstreamrefresh.NewClient(ctx),
			up.NewClient(ctx, vppConn),
			connectioncontext.NewClient(vppConn),
			memif.NewClient(ctx, vppConn),
			NewClient(ctx, &ifindex),
			sendfd.NewClient(),
			recvfd.NewClient(),
			excludedprefixes.NewClient(excludedprefixes.WithAwarenessGroups(config.AwarenessGroups)),
		),
		client.WithDialTimeout(config.DialTimeout),
		client.WithDialOptions(dialOptions...),
	)

	nsmClient = retry.NewClient(nsmClient, retry.WithTryTimeout(config.RequestTimeout))

	// ********************************************************************************
	// Configure signal handling context
	// ********************************************************************************
	signalCtx, cancelSignalCtx := notifyContext(ctx)
	defer cancelSignalCtx()

	// ********************************************************************************
	// Create Network Service Manager monitorClient
	// ********************************************************************************
	dialCtx, cancelDial := context.WithTimeout(signalCtx, config.DialTimeout)
	defer cancelDial()

	log.FromContext(ctx).Infof("NSC: Connecting to Network Service Manager %v", config.ConnectTo.String())
	cc, err := grpc.DialContext(dialCtx, grpcutils.URLToTarget(&config.ConnectTo), dialOptions...)
	if err != nil {
		log.FromContext(ctx).Fatalf("failed dial to NSMgr: %v", err.Error())
	}

	monitorClient := networkservice.NewMonitorConnectionClient(cc)

	// ********************************************************************************
	log.FromContext(ctx).Infof("executing phase 5: connect to all passed services (time since start: %s)", time.Since(starttime))
	// ********************************************************************************

	for i := 0; i < len(config.NetworkServices); i++ {
		u := nsurl.NSURL(config.NetworkServices[i])

		id := fmt.Sprintf("%s-%d", config.Name, i)
		var monitoredConnections map[string]*networkservice.Connection
		monitorCtx, cancelMonitor := context.WithTimeout(signalCtx, config.RequestTimeout)
		defer cancelMonitor()

		stream, err := monitorClient.MonitorConnections(monitorCtx, &networkservice.MonitorScopeSelector{
			PathSegments: []*networkservice.PathSegment{
				{
					Id: id,
				},
			},
		})
		if err != nil {
			log.FromContext(ctx).Fatalf("error from monitorConnectionClient", err.Error())
		}

		event, err := stream.Recv()
		if err != nil {
			log.FromContext(ctx).Errorf("error from monitorConnection stream", err.Error())
		} else {
			monitoredConnections = event.Connections
		}
		cancelMonitor()

		mech := u.Mechanism()
		if mech.Type != memif.MECHANISM {
			log.FromContext(ctx).Fatalf("mechanism type: %v is not supported", mech.Type)
		}
		request := &networkservice.NetworkServiceRequest{
			Connection: &networkservice.Connection{
				Id:             id,
				NetworkService: u.NetworkService(),
				Labels:         u.Labels(),
			},
			MechanismPreferences: []*networkservice.Mechanism{
				mech,
			},
		}

		for _, conn := range monitoredConnections {
			path := conn.GetPath()
			if path.Index == 1 && path.PathSegments[0].Id == id && conn.Mechanism.Type == u.Mechanism().Type {
				request.Connection = conn
				request.Connection.Path.Index = 0
				request.Connection.Id = id
				break
			}
		}

		resp, err := nsmClient.Request(ctx, request)
		if err != nil {
			log.FromContext(ctx).Fatalf("request has failed: %v", err.Error())
		}

		defer func() {
			closeCtx, cancelClose := context.WithTimeout(ctx, config.RequestTimeout)
			defer cancelClose()
			_, _ = nsmClient.Close(closeCtx, resp)
		}()
	}

	<-signalCtx.Done()
}

func exitOnErrCh(ctx context.Context, cancel context.CancelFunc, errCh <-chan error) {
	// If we already have an error, log it and exit
	select {
	case err := <-errCh:
		log.FromContext(ctx).Fatal(err)
	default:
	}
	// Otherwise wait for an error in the background to log and cancel
	go func(ctx context.Context, errCh <-chan error) {
		err := <-errCh
		log.FromContext(ctx).Error(err)
		cancel()
	}(ctx, errCh)
}

func notifyContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(
		ctx,
		os.Interrupt,
		// More Linux signals here
		syscall.SIGHUP,
		syscall.SIGTERM,
		syscall.SIGQUIT,
	)
}
