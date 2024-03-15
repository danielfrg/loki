/*
 *
 * Copyright 2021 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package xdsresource

import (
	"encoding/json"

	"google.golang.org/protobuf/types/known/anypb"
)

// ClusterType is the type of cluster from a received CDS response.
type ClusterType int

const (
	// ClusterTypeEDS represents the EDS cluster type, which will delegate endpoint
	// discovery to the management server.
	ClusterTypeEDS ClusterType = iota
	// ClusterTypeLogicalDNS represents the Logical DNS cluster type, which essentially
	// maps to the gRPC behavior of using the DNS resolver with pick_first LB policy.
	ClusterTypeLogicalDNS
	// ClusterTypeAggregate represents the Aggregate Cluster type, which provides a
	// prioritized list of clusters to use. It is used for failover between clusters
	// with a different configuration.
	ClusterTypeAggregate
)

// ClusterLRSServerConfigType is the type of LRS server config.
type ClusterLRSServerConfigType int

const (
	// ClusterLRSOff indicates LRS is off (loads are not reported for this
	// cluster).
	ClusterLRSOff ClusterLRSServerConfigType = iota
	// ClusterLRSServerSelf indicates loads should be reported to the same
	// server (the authority) where the CDS resp is received from.
	ClusterLRSServerSelf
)

// ClusterUpdate contains information from a received CDS response, which is of
// interest to the registered CDS watcher.
type ClusterUpdate struct {
	ClusterType ClusterType
	// ClusterName is the clusterName being watched for through CDS.
	ClusterName string
	// EDSServiceName is an optional name for EDS. If it's not set, the balancer
	// should watch ClusterName for the EDS resources.
	EDSServiceName string
	// LRSServerConfig contains the server where the load reports should be sent
	// to. This can be change to an interface, to support other types, e.g. a
	// ServerConfig with ServerURI, creds.
	LRSServerConfig ClusterLRSServerConfigType
	// SecurityCfg contains security configuration sent by the control plane.
	SecurityCfg *SecurityConfig
	// MaxRequests for circuit breaking, if any (otherwise nil).
	MaxRequests *uint32
	// DNSHostName is used only for cluster type DNS. It's the DNS name to
	// resolve in "host:port" form
	DNSHostName string
	// PrioritizedClusterNames is used only for cluster type aggregate. It represents
	// a prioritized list of cluster names.
	PrioritizedClusterNames []string

	// LBPolicy represents the locality and endpoint picking policy in JSON,
	// which will be the child policy of xds_cluster_impl.
	LBPolicy json.RawMessage

	// OutlierDetection is the outlier detection configuration for this cluster.
	// If nil, it means this cluster does not use the outlier detection feature.
	OutlierDetection json.RawMessage

	// Raw is the resource from the xds response.
	Raw *anypb.Any
}
