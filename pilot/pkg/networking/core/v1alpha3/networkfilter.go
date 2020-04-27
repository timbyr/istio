// Copyright 2018 Istio Authors
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

package v1alpha3

import (
	"time"

	core "github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	listener "github.com/envoyproxy/go-control-plane/envoy/api/v2/listener"
	accesslogconfig "github.com/envoyproxy/go-control-plane/envoy/config/accesslog/v2"
	accesslog "github.com/envoyproxy/go-control-plane/envoy/config/filter/accesslog/v2"
	kafka_broker "github.com/envoyproxy/go-control-plane/envoy/config/filter/network/kafka_broker/v2alpha1"
	mongo_proxy "github.com/envoyproxy/go-control-plane/envoy/config/filter/network/mongo_proxy/v2"
	mysql_proxy "github.com/envoyproxy/go-control-plane/envoy/config/filter/network/mysql_proxy/v1alpha1"
	redis_proxy "github.com/envoyproxy/go-control-plane/envoy/config/filter/network/redis_proxy/v2"
	tcp_proxy "github.com/envoyproxy/go-control-plane/envoy/config/filter/network/tcp_proxy/v2"
	thrift_proxy "github.com/envoyproxy/go-control-plane/envoy/config/filter/network/thrift_proxy/v2alpha1"
	zookeeper_proxy "github.com/envoyproxy/go-control-plane/envoy/config/filter/network/zookeeper_proxy/v1alpha1"
	"github.com/envoyproxy/go-control-plane/pkg/wellknown"
	"github.com/golang/protobuf/ptypes"

	networking "istio.io/api/networking/v1alpha3"

	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pilot/pkg/model"
	istio_route "istio.io/istio/pilot/pkg/networking/core/v1alpha3/route"
	"istio.io/istio/pilot/pkg/networking/util"
	"istio.io/istio/pkg/config/host"
	"istio.io/istio/pkg/config/protocol"
)

var (
	// redisOpTimeout is the default operation timeout for the Redis proxy filter.
	redisOpTimeout = 5 * time.Second

	// tcpGrpcAccessLog is used when access log service is enabled in mesh config.
	tcpGrpcAccessLog = buildTCPGrpcAccessLog()
)

// buildInboundNetworkFilters generates a TCP proxy network filter on the inbound path
func buildInboundNetworkFilters(push *model.PushContext, node *model.Proxy, instance *model.ServiceInstance) []*listener.Filter {
	clusterName := model.BuildSubsetKey(model.TrafficDirectionInbound, instance.ServicePort.Name,
		instance.Service.Hostname, instance.ServicePort.Port)
	statPrefix := clusterName
	// If stat name is configured, build the stat prefix from configured pattern.
	if len(push.Mesh.InboundClusterStatName) != 0 {
		statPrefix = util.BuildStatPrefix(push.Mesh.InboundClusterStatName, string(instance.Service.Hostname), "", instance.ServicePort, instance.Service.Attributes)
	}
	tcpProxy := &tcp_proxy.TcpProxy{
		StatPrefix:       statPrefix,
		ClusterSpecifier: &tcp_proxy.TcpProxy_Cluster{Cluster: clusterName},
	}
	tcpFilter := setAccessLogAndBuildTCPFilter(push, tcpProxy)
	return buildNetworkFiltersStack(node, instance.ServicePort, tcpFilter, statPrefix, clusterName)
}

// setAccessLog sets the AccessLog configuration in the given TcpProxy instance.
func setAccessLog(push *model.PushContext, config *tcp_proxy.TcpProxy) {
	if push.Mesh.AccessLogFile != "" {
		config.AccessLog = append(config.AccessLog, maybeBuildAccessLog(push.Mesh))
	}

	if push.Mesh.EnableEnvoyAccessLogService {
		config.AccessLog = append(config.AccessLog, tcpGrpcAccessLog)
	}
}

// setAccessLogAndBuildTCPFilter sets the AccessLog configuration in the given
// TcpProxy instance and builds a TCP filter out of it.
func setAccessLogAndBuildTCPFilter(push *model.PushContext, config *tcp_proxy.TcpProxy) *listener.Filter {
	setAccessLog(push, config)

	tcpFilter := &listener.Filter{
		Name:       wellknown.TCPProxy,
		ConfigType: &listener.Filter_TypedConfig{TypedConfig: util.MessageToAny(config)},
	}
	return tcpFilter
}

// buildOutboundNetworkFiltersWithSingleDestination takes a single cluster name
// and builds a stack of network filters.
func buildOutboundNetworkFiltersWithSingleDestination(push *model.PushContext, node *model.Proxy,
	statPrefix, clusterName string, port *model.Port) []*listener.Filter {

	tcpProxy := &tcp_proxy.TcpProxy{
		StatPrefix:       statPrefix,
		ClusterSpecifier: &tcp_proxy.TcpProxy_Cluster{Cluster: clusterName},
		// TODO: Need to set other fields such as Idle timeouts
	}

	idleTimeout, err := time.ParseDuration(node.Metadata.IdleTimeout)
	if idleTimeout > 0 && err == nil {
		tcpProxy.IdleTimeout = ptypes.DurationProto(idleTimeout)
	}

	tcpFilter := setAccessLogAndBuildTCPFilter(push, tcpProxy)
	return buildNetworkFiltersStack(node, port, tcpFilter, statPrefix, clusterName)
}

// buildOutboundNetworkFiltersWithWeightedClusters takes a set of weighted
// destination routes and builds a stack of network filters.
func buildOutboundNetworkFiltersWithWeightedClusters(node *model.Proxy, routes []*networking.RouteDestination,
	push *model.PushContext, port *model.Port, configMeta model.ConfigMeta) []*listener.Filter {

	statPrefix := configMeta.Name + "." + configMeta.Namespace
	clusterSpecifier := &tcp_proxy.TcpProxy_WeightedClusters{
		WeightedClusters: &tcp_proxy.TcpProxy_WeightedCluster{},
	}

	proxyConfig := &tcp_proxy.TcpProxy{
		StatPrefix:       statPrefix,
		ClusterSpecifier: clusterSpecifier,
		// TODO: Need to set other fields such as Idle timeouts
	}

	idleTimeout, err := time.ParseDuration(node.Metadata.IdleTimeout)
	if idleTimeout > 0 && err == nil {
		proxyConfig.IdleTimeout = ptypes.DurationProto(idleTimeout)
	}

	for _, route := range routes {
		service := node.SidecarScope.ServiceForHostname(host.Name(route.Destination.Host), push.ServiceByHostnameAndNamespace)
		if route.Weight > 0 {
			clusterName := istio_route.GetDestinationCluster(route.Destination, service, port.Port)
			clusterSpecifier.WeightedClusters.Clusters = append(clusterSpecifier.WeightedClusters.Clusters, &tcp_proxy.TcpProxy_WeightedCluster_ClusterWeight{
				Name:   clusterName,
				Weight: uint32(route.Weight),
			})
		}
	}

	// TODO: Need to handle multiple cluster names for Redis
	clusterName := clusterSpecifier.WeightedClusters.Clusters[0].Name
	tcpFilter := setAccessLogAndBuildTCPFilter(push, proxyConfig)
	return buildNetworkFiltersStack(node, port, tcpFilter, statPrefix, clusterName)
}

// buildNetworkFiltersStack builds a slice of network filters based on
// the protocol in use and the given TCP filter instance.
func buildNetworkFiltersStack(_ *model.Proxy, port *model.Port, tcpFilter *listener.Filter, statPrefix string, clusterName string) []*listener.Filter {
	filterstack := make([]*listener.Filter, 0)
	switch port.Protocol {
	case protocol.Mongo:
		filterstack = append(filterstack, buildMongoFilter(statPrefix), tcpFilter)
	case protocol.Redis:
		if features.EnableRedisFilter {
			// redis filter has route config, it is a terminating filter, no need append tcp filter.
			filterstack = append(filterstack, buildRedisFilter(statPrefix, clusterName))
		} else {
			filterstack = append(filterstack, tcpFilter)
		}
	case protocol.MySQL:
		if features.EnableMysqlFilter {
			filterstack = append(filterstack, buildMySQLFilter(statPrefix))
		}
		filterstack = append(filterstack, tcpFilter)
	case protocol.Thrift:
		if features.EnableThriftFilter {
			// Thrift filter has route config, it is a terminating filter, no need append tcp filter.
			filterstack = append(filterstack, buildThriftFilter(statPrefix))
		} else {
			filterstack = append(filterstack, tcpFilter)
		}
	case protocol.Kafka:
		if features.EnableKafkaFilter {
			filterstack = append(filterstack, buildKafkaFilter(statPrefix))
		} else {
			filterstack = append(filterstack, tcpFilter)
		}
	case protocol.ZooKeeper:
		if features.EnableZooKeeperFilter {
			filterstack = append(filterstack, buildZooKeeperFilter(statPrefix))
		} else {
			filterstack = append(filterstack, tcpFilter)
		}

	default:
		filterstack = append(filterstack, tcpFilter)
	}

	return filterstack
}

// buildOutboundNetworkFilters generates a TCP proxy network filter for outbound
// connections. In addition, it generates protocol specific filters (e.g., Mongo
// filter).
func buildOutboundNetworkFilters(node *model.Proxy,
	routes []*networking.RouteDestination, push *model.PushContext,
	port *model.Port, configMeta model.ConfigMeta) []*listener.Filter {
	if len(routes) == 1 {
		service := node.SidecarScope.ServiceForHostname(host.Name(routes[0].Destination.Host), push.ServiceByHostnameAndNamespace)
		clusterName := istio_route.GetDestinationCluster(routes[0].Destination, service, port.Port)
		statPrefix := clusterName
		// If stat name is configured, build the stat prefix from configured pattern.
		if len(push.Mesh.OutboundClusterStatName) != 0 && service != nil {
			statPrefix = util.BuildStatPrefix(push.Mesh.OutboundClusterStatName, routes[0].Destination.Host,
				routes[0].Destination.Subset, port, service.Attributes)
		}
		return buildOutboundNetworkFiltersWithSingleDestination(push, node, statPrefix, clusterName, port)
	}
	return buildOutboundNetworkFiltersWithWeightedClusters(node, routes, push, port, configMeta)
}

// buildThriftFilter builds an outbound Envoy Thrift filter.
func buildThriftFilter(statPrefix string) *listener.Filter {
	thriftProxy := &thrift_proxy.ThriftProxy{
		StatPrefix: statPrefix, // TODO (peter.novotnak@reddit.com) Thrift stats are prefixed with thrift.<statPrefix> by Envoy.
	}

	out := &listener.Filter{
		Name:       wellknown.ThriftProxy,
		ConfigType: &listener.Filter_TypedConfig{TypedConfig: util.MessageToAny(thriftProxy)},
	}

	return out
}

// buildMongoFilter builds an outbound Envoy MongoProxy filter.
func buildMongoFilter(statPrefix string) *listener.Filter {
	// TODO: add a watcher for /var/lib/istio/mongo/certs
	// if certs are found use, TLS or mTLS clusters for talking to MongoDB.
	// User is responsible for mounting those certs in the pod.
	mongoProxy := &mongo_proxy.MongoProxy{
		StatPrefix: statPrefix, // mongo stats are prefixed with mongo.<statPrefix> by Envoy
		// TODO enable faults in mongo
	}

	out := &listener.Filter{
		Name:       wellknown.MongoProxy,
		ConfigType: &listener.Filter_TypedConfig{TypedConfig: util.MessageToAny(mongoProxy)},
	}

	return out
}

// buildOutboundAutoPassthroughFilterStack builds a filter stack with sni_cluster and tcp_proxy
// used by auto_passthrough gateway servers
func buildOutboundAutoPassthroughFilterStack(push *model.PushContext, node *model.Proxy, port *model.Port) []*listener.Filter {
	// First build tcp_proxy with access logs
	// then add sni_cluster to the front
	tcpProxy := buildOutboundNetworkFiltersWithSingleDestination(push, node, util.BlackHoleCluster, util.BlackHoleCluster, port)
	filterstack := make([]*listener.Filter, 0)
	filterstack = append(filterstack, &listener.Filter{
		Name: util.SniClusterFilter,
	})
	filterstack = append(filterstack, tcpProxy...)

	return filterstack
}

// buildRedisFilter builds an outbound Envoy RedisProxy filter.
// Currently, if multiple clusters are defined, one of them will be picked for
// configuring the Redis proxy.
func buildRedisFilter(statPrefix, clusterName string) *listener.Filter {
	redisProxy := &redis_proxy.RedisProxy{
		LatencyInMicros: true,       // redis latency stats are captured in micro seconds which is typically the case.
		StatPrefix:      statPrefix, // redis stats are prefixed with redis.<statPrefix> by Envoy
		Settings: &redis_proxy.RedisProxy_ConnPoolSettings{
			OpTimeout: ptypes.DurationProto(redisOpTimeout), // TODO: Make this user configurable
		},
		PrefixRoutes: &redis_proxy.RedisProxy_PrefixRoutes{
			CatchAllRoute: &redis_proxy.RedisProxy_PrefixRoutes_Route{
				Cluster: clusterName,
			},
		},
	}

	out := &listener.Filter{
		Name:       wellknown.RedisProxy,
		ConfigType: &listener.Filter_TypedConfig{TypedConfig: util.MessageToAny(redisProxy)},
	}

	return out
}

// buildMySQLFilter builds an outbound Envoy MySQLProxy filter.
func buildMySQLFilter(statPrefix string) *listener.Filter {
	mySQLProxy := &mysql_proxy.MySQLProxy{
		StatPrefix: statPrefix, // MySQL stats are prefixed with mysql.<statPrefix> by Envoy.
	}

	out := &listener.Filter{
		Name:       wellknown.MySQLProxy,
		ConfigType: &listener.Filter_TypedConfig{TypedConfig: util.MessageToAny(mySQLProxy)},
	}

	return out
}

// buildKafkaFilter builds an outbound Envoy KafkaProxy filter.
func buildKafkaFilter(statPrefix string) *listener.Filter {
	kafkaBroker := &kafka_broker.KafkaBroker{
		StatPrefix: statPrefix, // Kafka stats are prefixed with kafka.<statPrefix> by Envoy.
	}

	out := &listener.Filter{
		Name:       "envoy.filters.network.kafka_broker",
		ConfigType: &listener.Filter_TypedConfig{TypedConfig: util.MessageToAny(kafkaBroker)},
	}

	return out
}

// buildZookeeperFilter builds an outbound Envoy ZookeeperProxy filter.
func buildZooKeeperFilter(statPrefix string) *listener.Filter {
	zooKeeperProxy := &zookeeper_proxy.ZooKeeperProxy{
		StatPrefix: statPrefix, // Zookeeper stats are prefixed with zookeeper.<statPrefix> by Envoy.
	}

	out := &listener.Filter{
		Name:       "envoy.filters.network.zookeeper_proxy",
		ConfigType: &listener.Filter_TypedConfig{TypedConfig: util.MessageToAny(zooKeeperProxy)},
	}

	return out
}

func buildTCPGrpcAccessLog() *accesslog.AccessLog {
	fl := &accesslogconfig.TcpGrpcAccessLogConfig{
		CommonConfig: &accesslogconfig.CommonGrpcAccessLogConfig{
			LogName: tcpEnvoyAccessLogFriendlyName,
			GrpcService: &core.GrpcService{
				TargetSpecifier: &core.GrpcService_EnvoyGrpc_{
					EnvoyGrpc: &core.GrpcService_EnvoyGrpc{
						ClusterName: EnvoyAccessLogCluster,
					},
				},
			},
		},
	}

	fl.CommonConfig.FilterStateObjectsToLog = envoyWasmStateToLog

	return &accesslog.AccessLog{
		Name:       tcpEnvoyALSName,
		ConfigType: &accesslog.AccessLog_TypedConfig{TypedConfig: util.MessageToAny(fl)},
	}
}
