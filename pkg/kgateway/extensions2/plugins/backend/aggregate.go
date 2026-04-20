package backend

import (
	"fmt"

	envoyclusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	aggregatev3 "github.com/envoyproxy/go-control-plane/envoy/extensions/clusters/aggregate/v3"
	"istio.io/istio/pkg/kube/krt"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/kgateway-dev/kgateway/v2/api/v1alpha1/kgateway"
	"github.com/kgateway-dev/kgateway/v2/pkg/kgateway/utils"
	"github.com/kgateway-dev/kgateway/v2/pkg/krtcollections"
	"github.com/kgateway-dev/kgateway/v2/pkg/pluginsdk/ir"
	"github.com/kgateway-dev/kgateway/v2/pkg/utils/cmputils"
)

const (
	aggregateClusterExtensionName = "envoy.clusters.aggregate"
	// kubeServiceClusterPrefix is the GvPrefix used by the kubernetes service plugin.
	// It must match the BackendClusterPrefix constant in pkg/kgateway/extensions2/plugins/kubernetes.
	kubeServiceClusterPrefix = "kube"
)

// AggregateIr is the internal representation of an aggregate backend.
type AggregateIr struct {
	// ClusterNames holds the resolved Envoy cluster names in priority order.
	// +noKrtEquals
	ClusterNames []string
}

// Equals checks if two AggregateIr objects are equal.
func (u *AggregateIr) Equals(other any) bool {
	otherAggregate, ok := other.(*AggregateIr)
	if !ok {
		return false
	}
	return cmputils.CompareWithNils(u, otherAggregate, func(a, b *AggregateIr) bool {
		if len(a.ClusterNames) != len(b.ClusterNames) {
			return false
		}
		for i := range a.ClusterNames {
			if a.ClusterNames[i] != b.ClusterNames[i] {
				return false
			}
		}
		return true
	})
}

// buildAggregateIr resolves each member reference to a deterministic Envoy cluster name.
//
// Cluster names are computed directly from the member ref (without going through the
// BackendIndex) to avoid a KRT circular dependency — the aggregate Backend's KRT
// collection is itself registered in the BackendIndex, so reading from the index inside
// that same collection would create a cycle.
//
// Existence and ReferenceGrant validation is performed against the raw backend and
// service collections, which do not depend on the aggregate Backend collection.
func buildAggregateIr(
	krtctx krt.HandlerContext,
	b *kgateway.Backend,
	rawBackends krt.Collection[*kgateway.Backend],
	services krt.Collection[*corev1.Service],
	refGrants *krtcollections.RefGrantIndex,
) (*AggregateIr, []error) {
	fromSrc := ir.ObjectSource{
		Group:     kgateway.GroupVersion.Group,
		Kind:      "Backend",
		Namespace: b.GetNamespace(),
		Name:      b.GetName(),
	}

	aggregateIr := &AggregateIr{}
	var errs []error

	for i, ref := range b.Spec.Aggregate.BackendRefs {
		memberGroup := ""
		if ref.Group != nil {
			memberGroup = string(*ref.Group)
		}
		memberKind := "Service"
		if ref.Kind != nil {
			memberKind = string(*ref.Kind)
		}
		memberNs := b.GetNamespace()
		if ref.Namespace != nil {
			memberNs = string(*ref.Namespace)
		}
		memberName := string(ref.Name)

		// Self-reference guard.
		if memberGroup == kgateway.GroupVersion.Group &&
			memberKind == "Backend" &&
			memberNs == b.GetNamespace() &&
			memberName == b.GetName() {
			errs = append(errs, fmt.Errorf("member[%d]: aggregate backend cannot reference itself", i))
			continue
		}

		// Validate ReferenceGrant for cross-namespace references.
		toSrc := ir.ObjectSource{
			Group:     memberGroup,
			Kind:      memberKind,
			Namespace: memberNs,
			Name:      memberName,
		}
		if !refGrants.ReferenceAllowed(krtctx, fromSrc.GetGroupKind(), fromSrc.Namespace, toSrc) {
			errs = append(errs, fmt.Errorf("member[%d] %s/%s: %w", i, memberNs, memberName, krtcollections.ErrMissingReferenceGrant))
			continue
		}

		// Resolve to an Envoy cluster name and validate existence.
		var clusterName string
		switch {
		case memberGroup == kgateway.GroupVersion.Group && memberKind == "Backend":
			nn := types.NamespacedName{Namespace: memberNs, Name: memberName}
			if existing := krt.FetchOne(krtctx, rawBackends, krt.FilterKey(nn.String())); existing == nil {
				errs = append(errs, fmt.Errorf("member[%d]: Backend %s/%s not found", i, memberNs, memberName))
				continue
			}
			// Cluster name format mirrors BackendObjectIR.ClusterName():
			// "{gvPrefix}_{namespace}_{name}_{port}" with gvPrefix=ExtensionName ("backend"), port=0.
			clusterName = fmt.Sprintf("%s_%s_%s_0", ExtensionName, memberNs, memberName)

		default:
			// Default: Kubernetes Service.
			var memberPort int32
			if ref.Port != nil {
				memberPort = int32(*ref.Port) //nolint:gosec // G115: PortNumber is int32 1-65535
			}
			nn := types.NamespacedName{Namespace: memberNs, Name: memberName}
			svc := krt.FetchOne(krtctx, services, krt.FilterKey(nn.String()))
			if svc == nil {
				errs = append(errs, fmt.Errorf("member[%d]: Service %s/%s not found", i, memberNs, memberName))
				continue
			}
			// Validate that the requested port exists on the service.
			portFound := false
			for _, sp := range (*svc).Spec.Ports {
				if sp.Port == memberPort {
					portFound = true
					break
				}
			}
			if !portFound {
				errs = append(errs, fmt.Errorf("member[%d]: Service %s/%s has no port %d", i, memberNs, memberName, memberPort))
				continue
			}
			// Cluster name format mirrors BackendObjectIR.ClusterName():
			// "{gvPrefix}_{namespace}_{name}_{port}" with gvPrefix=kubeServiceClusterPrefix ("kube").
			clusterName = fmt.Sprintf("%s_%s_%s_%d", kubeServiceClusterPrefix, memberNs, memberName, memberPort)
		}

		aggregateIr.ClusterNames = append(aggregateIr.ClusterNames, clusterName)
	}

	return aggregateIr, errs
}

// processAggregate applies the aggregate IR to the envoy cluster, configuring it as
// an envoy.clusters.aggregate custom cluster type with the resolved cluster names.
func processAggregate(aggregateIr *AggregateIr, out *envoyclusterv3.Cluster) {
	out.LbPolicy = envoyclusterv3.Cluster_CLUSTER_PROVIDED

	clusterNames := make([]string, len(aggregateIr.ClusterNames))
	copy(clusterNames, aggregateIr.ClusterNames)

	cfg := &aggregatev3.ClusterConfig{
		Clusters: clusterNames,
	}
	typedConfig, err := utils.MessageToAny(cfg)
	if err != nil {
		logger.Error("failed to marshal aggregate cluster config", "error", err)
		return
	}

	out.ClusterDiscoveryType = &envoyclusterv3.Cluster_ClusterType{
		ClusterType: &envoyclusterv3.Cluster_CustomClusterType{
			Name:        aggregateClusterExtensionName,
			TypedConfig: typedConfig,
		},
	}
}
