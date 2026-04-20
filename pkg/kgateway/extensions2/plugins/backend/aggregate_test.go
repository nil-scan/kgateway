package backend

import (
	"testing"

	envoyclusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	aggregatev3 "github.com/envoyproxy/go-control-plane/envoy/extensions/clusters/aggregate/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"istio.io/istio/pkg/kube/krt"
	"istio.io/istio/pkg/kube/krt/krttest"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwv1b1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/kgateway-dev/kgateway/v2/api/v1alpha1/kgateway"
	kgwwellknown "github.com/kgateway-dev/kgateway/v2/pkg/kgateway/wellknown"
	"github.com/kgateway-dev/kgateway/v2/pkg/krtcollections"
)

// buildTestCollections creates mock KRT collections used by buildAggregateIr from a
// list of objects. Supported types: *kgateway.Backend, *corev1.Service,
// *gwv1b1.ReferenceGrant.
func buildTestCollections(t *testing.T, inputs []any) (
	rawBackends krt.Collection[*kgateway.Backend],
	services krt.Collection[*corev1.Service],
	refGrants *krtcollections.RefGrantIndex,
) {
	t.Helper()
	mock := krttest.NewMock(t, inputs)
	rawBackends = krttest.GetMockCollection[*kgateway.Backend](mock)
	services = krttest.GetMockCollection[*corev1.Service](mock)
	refGrantCol := krttest.GetMockCollection[*gwv1b1.ReferenceGrant](mock)
	refGrants = krtcollections.NewRefGrantIndex(refGrantCol)
	return rawBackends,
		services,
		refGrants
}

func TestAggregateIrEquals(t *testing.T) {
	t.Run("nil equals nil", func(t *testing.T) {
		var a, b *AggregateIr
		assert.True(t, a.Equals(b))
	})

	t.Run("nil does not equal non-nil", func(t *testing.T) {
		var a *AggregateIr
		b := &AggregateIr{ClusterNames: []string{"cluster-a"}}
		assert.False(t, a.Equals(b))
	})

	t.Run("same cluster names are equal", func(t *testing.T) {
		a := &AggregateIr{ClusterNames: []string{"cluster-a", "cluster-b"}}
		b := &AggregateIr{ClusterNames: []string{"cluster-a", "cluster-b"}}
		assert.True(t, a.Equals(b))
	})

	t.Run("different cluster names are not equal", func(t *testing.T) {
		a := &AggregateIr{ClusterNames: []string{"cluster-a", "cluster-b"}}
		b := &AggregateIr{ClusterNames: []string{"cluster-a", "cluster-c"}}
		assert.False(t, a.Equals(b))
	})

	t.Run("different lengths are not equal", func(t *testing.T) {
		a := &AggregateIr{ClusterNames: []string{"cluster-a"}}
		b := &AggregateIr{ClusterNames: []string{"cluster-a", "cluster-b"}}
		assert.False(t, a.Equals(b))
	})

	t.Run("wrong type returns false", func(t *testing.T) {
		a := &AggregateIr{ClusterNames: []string{"cluster-a"}}
		assert.False(t, a.Equals("not-an-aggregate-ir"))
	})
}

func TestProcessAggregate(t *testing.T) {
	t.Run("sets CLUSTER_PROVIDED lb policy", func(t *testing.T) {
		aIr := &AggregateIr{ClusterNames: []string{"cluster-a", "cluster-b"}}
		cluster := &envoyclusterv3.Cluster{Name: "my-aggregate"}
		processAggregate(aIr, cluster)
		assert.Equal(t, envoyclusterv3.Cluster_CLUSTER_PROVIDED, cluster.LbPolicy)
	})

	t.Run("sets aggregate custom cluster type", func(t *testing.T) {
		aIr := &AggregateIr{ClusterNames: []string{"cluster-a", "cluster-b"}}
		cluster := &envoyclusterv3.Cluster{Name: "my-aggregate"}
		processAggregate(aIr, cluster)

		clusterType := cluster.GetClusterType()
		require.NotNil(t, clusterType)
		assert.Equal(t, aggregateClusterExtensionName, clusterType.GetName())

		var cfg aggregatev3.ClusterConfig
		err := proto.UnmarshalOptions{}.Unmarshal(clusterType.GetTypedConfig().GetValue(), &cfg)
		require.NoError(t, err)
		assert.Equal(t, []string{"cluster-a", "cluster-b"}, cfg.GetClusters())
	})

	t.Run("proto output is independent of source slice", func(t *testing.T) {
		aIr := &AggregateIr{ClusterNames: []string{"cluster-a", "cluster-b"}}
		cluster := &envoyclusterv3.Cluster{Name: "my-aggregate"}
		processAggregate(aIr, cluster)

		// Mutate the original slice; the encoded proto should still have the original values.
		aIr.ClusterNames[0] = "mutated"

		var cfg aggregatev3.ClusterConfig
		err := proto.UnmarshalOptions{}.Unmarshal(cluster.GetClusterType().GetTypedConfig().GetValue(), &cfg)
		require.NoError(t, err)
		assert.Equal(t, "cluster-a", cfg.GetClusters()[0], "proto output should not be affected by later mutations to the IR slice")
	})
}

func TestBuildAggregateIr_SameNamespaceService(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "svc-primary", Namespace: "default"},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 8080}}},
	}
	svc2 := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "svc-secondary", Namespace: "default"},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 8080}}},
	}

	rawBackends, services, refGrants := buildTestCollections(t, []any{svc, svc2})

	b := &kgateway.Backend{
		ObjectMeta: metav1.ObjectMeta{Name: "agg", Namespace: "default"},
		Spec: kgateway.BackendSpec{
			Aggregate: &kgateway.AggregateBackend{
				BackendRefs: []gwv1.BackendObjectReference{
					{Name: "svc-primary", Port: ptr.To(gwv1.PortNumber(8080))},
					{Name: "svc-secondary", Port: ptr.To(gwv1.PortNumber(8080))},
				},
			},
		},
	}

	aIr, errs := buildAggregateIr(krt.TestingDummyContext{}, b, rawBackends, services, refGrants)
	require.Empty(t, errs)
	require.NotNil(t, aIr)
	require.Len(t, aIr.ClusterNames, 2)
	assert.Equal(t, "kube_default_svc-primary_8080", aIr.ClusterNames[0])
	assert.Equal(t, "kube_default_svc-secondary_8080", aIr.ClusterNames[1])
}

func TestBuildAggregateIr_SameNamespaceBackend(t *testing.T) {
	member1 := &kgateway.Backend{
		ObjectMeta: metav1.ObjectMeta{Name: "primary", Namespace: "default"},
		Spec:       kgateway.BackendSpec{Static: &kgateway.StaticBackend{Hosts: []kgateway.Host{{Host: "primary.example.com", Port: 443}}}},
	}
	member2 := &kgateway.Backend{
		ObjectMeta: metav1.ObjectMeta{Name: "secondary", Namespace: "default"},
		Spec:       kgateway.BackendSpec{Static: &kgateway.StaticBackend{Hosts: []kgateway.Host{{Host: "secondary.example.com", Port: 443}}}},
	}

	rawBackends, services, refGrants := buildTestCollections(t, []any{member1, member2})

	b := &kgateway.Backend{
		ObjectMeta: metav1.ObjectMeta{Name: "agg", Namespace: "default"},
		Spec: kgateway.BackendSpec{
			Aggregate: &kgateway.AggregateBackend{
				BackendRefs: []gwv1.BackendObjectReference{
					{
						Group: new(gwv1.Group(kgwwellknown.BackendGVK.Group)),
						Kind:  ptr.To(gwv1.Kind("Backend")),
						Name:  "primary",
					},
					{
						Group: new(gwv1.Group(kgwwellknown.BackendGVK.Group)),
						Kind:  ptr.To(gwv1.Kind("Backend")),
						Name:  "secondary",
					},
				},
			},
		},
	}

	aIr, errs := buildAggregateIr(krt.TestingDummyContext{}, b, rawBackends, services, refGrants)
	require.Empty(t, errs)
	require.NotNil(t, aIr)
	require.Len(t, aIr.ClusterNames, 2)
	assert.Equal(t, "backend_default_primary_0", aIr.ClusterNames[0])
	assert.Equal(t, "backend_default_secondary_0", aIr.ClusterNames[1])
}

func TestBuildAggregateIr_UnresolvableMember(t *testing.T) {
	rawBackends, services, refGrants := buildTestCollections(t, []any{})

	b := &kgateway.Backend{
		ObjectMeta: metav1.ObjectMeta{Name: "agg", Namespace: "default"},
		Spec: kgateway.BackendSpec{
			Aggregate: &kgateway.AggregateBackend{
				BackendRefs: []gwv1.BackendObjectReference{
					{Name: "missing-svc", Port: ptr.To(gwv1.PortNumber(8080))},
				},
			},
		},
	}

	aIr, errs := buildAggregateIr(krt.TestingDummyContext{}, b, rawBackends, services, refGrants)
	require.NotEmpty(t, errs, "expected error for unresolvable member")
	assert.Empty(t, aIr.ClusterNames)
}

func TestBuildAggregateIr_CrossNamespaceMissingRefGrant(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "remote-svc", Namespace: "other"},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 8080}}},
	}

	rawBackends, services, refGrants := buildTestCollections(t, []any{svc})

	b := &kgateway.Backend{
		ObjectMeta: metav1.ObjectMeta{Name: "agg", Namespace: "default"},
		Spec: kgateway.BackendSpec{
			Aggregate: &kgateway.AggregateBackend{
				BackendRefs: []gwv1.BackendObjectReference{
					{
						Name:      "remote-svc",
						Namespace: ptr.To(gwv1.Namespace("other")),
						Port:      ptr.To(gwv1.PortNumber(8080)),
					},
				},
			},
		},
	}

	aIr, errs := buildAggregateIr(krt.TestingDummyContext{}, b, rawBackends, services, refGrants)
	require.NotEmpty(t, errs, "expected error for missing ReferenceGrant")
	require.Len(t, errs, 1)
	assert.ErrorContains(t, errs[0], "missing reference grant")
	assert.Empty(t, aIr.ClusterNames)
}

func TestBuildAggregateIr_SelfReferenceRejected(t *testing.T) {
	b := &kgateway.Backend{
		ObjectMeta: metav1.ObjectMeta{Name: "agg", Namespace: "default"},
		Spec: kgateway.BackendSpec{
			Aggregate: &kgateway.AggregateBackend{
				BackendRefs: []gwv1.BackendObjectReference{
					{
						Group: new(gwv1.Group(kgwwellknown.BackendGVK.Group)),
						Kind:  ptr.To(gwv1.Kind("Backend")),
						Name:  "agg", // same name as parent
					},
				},
			},
		},
	}

	rawBackends, services, refGrants := buildTestCollections(t, []any{b})

	aIr, errs := buildAggregateIr(krt.TestingDummyContext{}, b, rawBackends, services, refGrants)
	require.NotEmpty(t, errs, "expected error for self-reference")
	assert.ErrorContains(t, errs[0], "cannot reference itself")
	assert.Empty(t, aIr.ClusterNames)
}
