// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/cortexproject/cortex/blob/master/pkg/querier/store_gateway_client.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Cortex Authors.

package querier

import (
	"context"
	"flag"
	"time"

	"github.com/go-kit/log"
	"github.com/grafana/dskit/crypto/tls"
	"github.com/grafana/dskit/grpcclient"
	"github.com/grafana/dskit/ring/client"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health/grpc_health_v1"

	"github.com/grafana/mimir/pkg/storegateway/storegatewaypb"
)

func newStoreGatewayClientFactory(clientCfg grpcclient.Config, dialTimeout time.Duration, reg prometheus.Registerer) client.PoolFactory {
	requestDuration := promauto.With(reg).NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   "cortex",
		Name:        "storegateway_client_request_duration_seconds",
		Help:        "Time spent executing requests to the store-gateway.",
		Buckets:     prometheus.ExponentialBuckets(0.008, 4, 7),
		ConstLabels: prometheus.Labels{"client": "querier"},
	}, []string{"operation", "status_code"})

	return func(addr string) (client.PoolClient, error) {
		return dialStoreGatewayClient(clientCfg, addr, dialTimeout, requestDuration)
	}
}

func dialStoreGatewayClient(clientCfg grpcclient.Config, addr string, dialTimeout time.Duration, requestDuration *prometheus.HistogramVec) (*storeGatewayClient, error) {
	opts, err := clientCfg.DialOption(grpcclient.Instrument(requestDuration))
	if err != nil {
		return nil, err
	}
	opts = append(opts, grpc.WithBlock())

	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()

	conn, err := grpc.DialContext(ctx, addr, opts...)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to dial store-gateway %s", addr)
	}

	return &storeGatewayClient{
		StoreGatewayClient: storegatewaypb.NewStoreGatewayClient(conn),
		HealthClient:       grpc_health_v1.NewHealthClient(conn),
		conn:               conn,
	}, nil
}

type storeGatewayClient struct {
	storegatewaypb.StoreGatewayClient
	grpc_health_v1.HealthClient
	conn *grpc.ClientConn
}

func (c *storeGatewayClient) Close() error {
	return c.conn.Close()
}

func (c *storeGatewayClient) String() string {
	return c.RemoteAddress()
}

func (c *storeGatewayClient) RemoteAddress() string {
	return c.conn.Target()
}

func newStoreGatewayClientPool(discovery client.PoolServiceDiscovery, clientConfig ClientConfig, logger log.Logger, reg prometheus.Registerer) *client.Pool {
	// We prefer sane defaults instead of exposing further config options.
	clientCfg := grpcclient.Config{
		MaxRecvMsgSize:      100 << 20,
		MaxSendMsgSize:      16 << 20,
		GRPCCompression:     "",
		RateLimit:           0,
		RateLimitBurst:      0,
		BackoffOnRatelimits: false,
		TLSEnabled:          clientConfig.TLSEnabled,
		TLS:                 clientConfig.TLS,
	}
	poolCfg := client.PoolConfig{
		CheckInterval:      clientConfig.PoolConfig.CleanupPeriod,
		HealthCheckEnabled: true,
		HealthCheckTimeout: clientConfig.PoolConfig.RemoteTimeout,
	}

	clientsCount := promauto.With(reg).NewGauge(prometheus.GaugeOpts{
		Namespace:   "cortex",
		Name:        "storegateway_clients",
		Help:        "The current number of store-gateway clients in the pool.",
		ConstLabels: map[string]string{"client": "querier"},
	})

	return client.NewPool("store-gateway", poolCfg, discovery, newStoreGatewayClientFactory(clientCfg, poolCfg.HealthCheckTimeout, reg), clientsCount, logger)
}

// PoolConfig is config for creating a Pool of gRPC clients.
type PoolConfig struct {
	CleanupPeriod time.Duration `yaml:"cleanup_period" category:"advanced"`
	RemoteTimeout time.Duration `yaml:"remote_timeout" category:"advanced"`
}

type ClientConfig struct {
	TLSEnabled bool             `yaml:"tls_enabled" category:"advanced"`
	TLS        tls.ClientConfig `yaml:",inline"`
	PoolConfig PoolConfig       `yaml:",inline"`
}

func (cfg *ClientConfig) RegisterFlagsWithPrefix(prefix string, f *flag.FlagSet) {
	f.BoolVar(&cfg.TLSEnabled, prefix+".tls-enabled", cfg.TLSEnabled, "Enable TLS for gRPC client connecting to store-gateway.")
	f.DurationVar(&cfg.PoolConfig.CleanupPeriod, prefix+".cleanup-period", 5*time.Second, "How frequently to clean up clients for store-gateways that have gone away.")
	f.DurationVar(&cfg.PoolConfig.RemoteTimeout, prefix+".remote-timeout", time.Second, "Timeout for detecting a store-gateway that has gone away.")
	cfg.TLS.RegisterFlagsWithPrefix(prefix, f)
}
