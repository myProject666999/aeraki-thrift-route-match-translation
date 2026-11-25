// Copyright Aeraki Authors
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

package thrift

import (
	"testing"

	routepb "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	thrift "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/thrift_proxy/v3"
	matcher "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	"github.com/golang/protobuf/ptypes/wrappers"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"
	networking "istio.io/api/networking/v1alpha3"

	"github.com/aeraki-mesh/aeraki/internal/model"
)

const (
	testHost = "thrift-sample-server.thrift.svc.cluster.local"
	testPort = 9090
)

func newContext(vs *networking.VirtualService) *model.EnvoyFilterContext {
	context := &model.EnvoyFilterContext{
		ServiceEntry: &model.ServiceEntryWrapper{
			Spec: &networking.ServiceEntry{
				Hosts: []string{testHost},
				Ports: []*networking.ServicePort{
					{Number: testPort, Protocol: "TCP", Name: "tcp-thrift-hello-server"},
				},
			},
		},
	}
	if vs != nil {
		context.VirtualService = &model.VirtualServiceWrapper{Spec: vs}
	}
	return context
}

func clusterNameFor(subset string) string {
	return model.BuildClusterName(model.TrafficDirectionOutbound, subset, testHost, testPort)
}

func routeToMethod(methodName string, headers []*routepb.HeaderMatcher, cluster string) *thrift.Route {
	return &thrift.Route{
		Match: &thrift.RouteMatch{
			MatchSpecifier: &thrift.RouteMatch_MethodName{MethodName: methodName},
			Headers:        headers,
		},
		Route: &thrift.RouteAction{
			ClusterSpecifier: &thrift.RouteAction_Cluster{Cluster: cluster},
		},
	}
}

func exactHeader(name, value string) *routepb.HeaderMatcher {
	return &routepb.HeaderMatcher{
		Name:                 name,
		HeaderMatchSpecifier: &routepb.HeaderMatcher_ExactMatch{ExactMatch: value},
	}
}

func routeConfiguration(want ...*thrift.Route) *thrift.RouteConfiguration {
	return &thrift.RouteConfiguration{Name: clusterNameFor(""), Routes: want}
}

func TestBuildOutboundRouteConfig(t *testing.T) {
	testCases := []struct {
		name string
		vs   *networking.VirtualService
		want *thrift.RouteConfiguration
	}{
		{
			name: "no virtual service",
			want: routeConfiguration(routeToMethod("", nil, clusterNameFor(""))),
		},
		{
			name: "single rule without match",
			vs: &networking.VirtualService{
				Http: []*networking.HTTPRoute{
					{
						Name:  "thrift-v1-routes",
						Route: []*networking.HTTPRouteDestination{destination("v1", 0)},
					},
				},
			},
			want: routeConfiguration(routeToMethod("", nil, clusterNameFor("v1"))),
		},
		{
			name: "traffic splitting without match",
			vs: &networking.VirtualService{
				Http: []*networking.HTTPRoute{
					{
						Name: "thrift-traffic-splitting",
						Route: []*networking.HTTPRouteDestination{
							destination("v1", 20),
							destination("v2", 80),
						},
					},
				},
			},
			want: routeConfiguration(&thrift.Route{
				Match: matchAllMethods(),
				Route: &thrift.RouteAction{
					ClusterSpecifier: &thrift.RouteAction_WeightedClusters{
						WeightedClusters: &thrift.WeightedCluster{
							Clusters: []*thrift.WeightedCluster_ClusterWeight{
								{Name: clusterNameFor("v1"), Weight: &wrappers.UInt32Value{Value: 20}},
								{Name: clusterNameFor("v2"), Weight: &wrappers.UInt32Value{Value: 80}},
							},
						},
					},
				},
			}),
		},
		{
			name: "method exact match followed by catch-all keeps the rule order",
			vs: &networking.VirtualService{
				Http: []*networking.HTTPRoute{
					{
						Name:  "get-user-v2",
						Match: []*networking.HTTPMatchRequest{{Method: exact("getUser")}},
						Route: []*networking.HTTPRouteDestination{destination("v2", 0)},
					},
					{
						Name:  "default-v1",
						Route: []*networking.HTTPRouteDestination{destination("v1", 0)},
					},
				},
			},
			want: routeConfiguration(
				routeToMethod("getUser", nil, clusterNameFor("v2")),
				routeToMethod("", nil, clusterNameFor("v1")),
			),
		},
		{
			name: "method prefix match drops only the unmappable rule",
			vs: &networking.VirtualService{
				Http: []*networking.HTTPRoute{
					{
						Name:  "get-v2",
						Match: []*networking.HTTPMatchRequest{{Method: prefix("get")}},
						Route: []*networking.HTTPRouteDestination{destination("v2", 0)},
					},
					{
						Name:  "default-v1",
						Route: []*networking.HTTPRouteDestination{destination("v1", 0)},
					},
				},
			},
			want: routeConfiguration(routeToMethod("", nil, clusterNameFor("v1"))),
		},
		{
			name: "method regex match drops only the unmappable rule",
			vs: &networking.VirtualService{
				Http: []*networking.HTTPRoute{
					{
						Name:  "get-v2",
						Match: []*networking.HTTPMatchRequest{{Method: regex("get.*")}},
						Route: []*networking.HTTPRouteDestination{destination("v2", 0)},
					},
					{
						Name:  "default-v1",
						Route: []*networking.HTTPRouteDestination{destination("v1", 0)},
					},
				},
			},
			want: routeConfiguration(routeToMethod("", nil, clusterNameFor("v1"))),
		},
		{
			name: "a virtual service with only an unmappable rule falls back to the default route",
			vs: &networking.VirtualService{
				Http: []*networking.HTTPRoute{
					{
						Name:  "get-v2",
						Match: []*networking.HTTPMatchRequest{{Method: prefix("get")}},
						Route: []*networking.HTTPRouteDestination{destination("v2", 0)},
					},
				},
			},
			want: routeConfiguration(routeToMethod("", nil, clusterNameFor(""))),
			// The fallback points at the base cluster, not at the subset of the dropped rule.
		},
		{
			name: "multiple match elements are expanded into multiple routes",
			vs: &networking.VirtualService{
				Http: []*networking.HTTPRoute{
					{
						Name: "two-ways-to-v2",
						Match: []*networking.HTTPMatchRequest{
							{Method: exact("getUser")},
							{Headers: map[string]*networking.StringMatch{"x-env": exact("prod")}},
						},
						Route: []*networking.HTTPRouteDestination{destination("v2", 0)},
					},
				},
			},
			// Both elements get their own route, and the rule order is preserved.
			want: routeConfiguration(
				routeToMethod("getUser", nil, clusterNameFor("v2")),
				routeToMethod("", []*routepb.HeaderMatcher{exactHeader("x-env", "prod")}, clusterNameFor("v2")),
			),
		},
		{
			name: "a single match element with method and headers yields one route",
			vs: &networking.VirtualService{
				Http: []*networking.HTTPRoute{
					{
						Name: "user-in-prod",
						Match: []*networking.HTTPMatchRequest{
							{
								Method: exact("getUser"),
								Headers: map[string]*networking.StringMatch{
									"x-env":    exact("prod"),
									"x-region": prefix("cn-"),
									"x-trace":  regex("[0-9a-f]{32}"),
								},
							},
						},
						Route: []*networking.HTTPRouteDestination{destination("v2", 0)},
					},
				},
			},
			// Header names are sorted so that the generated configuration is stable.
			want: routeConfiguration(routeToMethod("getUser", []*routepb.HeaderMatcher{
				exactHeader("x-env", "prod"),
				{
					Name:                 "x-region",
					HeaderMatchSpecifier: &routepb.HeaderMatcher_PrefixMatch{PrefixMatch: "cn-"},
				},
				{
					Name: "x-trace",
					HeaderMatchSpecifier: &routepb.HeaderMatcher_SafeRegexMatch{
						SafeRegexMatch: &matcher.RegexMatcher{
							EngineType: regexEngine,
							Regex:      "[0-9a-f]{32}",
						},
					},
				},
			}, clusterNameFor("v2"))),
		},
		{
			name: "http only fields are ignored when a thrift expressible condition exists",
			vs: &networking.VirtualService{
				Http: []*networking.HTTPRoute{
					{
						Name: "get-user",
						Match: []*networking.HTTPMatchRequest{
							{Method: exact("getUser"), Uri: exact("/getUser")},
						},
						Route: []*networking.HTTPRouteDestination{destination("v2", 0)},
					},
				},
			},
			want: routeConfiguration(routeToMethod("getUser", nil, clusterNameFor("v2"))),
		},
		{
			name: "a match element with only http specific fields is dropped, not widened to match-all",
			vs: &networking.VirtualService{
				Http: []*networking.HTTPRoute{
					{
						Name: "uri-only",
						Match: []*networking.HTTPMatchRequest{
							{Uri: exact("/getUser"), Authority: exact(testHost)},
						},
						Route: []*networking.HTTPRouteDestination{destination("v2", 0)},
					},
					{
						Name:  "default-v1",
						Route: []*networking.HTTPRouteDestination{destination("v1", 0)},
					},
				},
			},
			want: routeConfiguration(routeToMethod("", nil, clusterNameFor("v1"))),
		},
		{
			name: "an empty method exact match is dropped instead of silently matching all methods",
			vs: &networking.VirtualService{
				Http: []*networking.HTTPRoute{
					{
						Name:  "empty-method-v2",
						Match: []*networking.HTTPMatchRequest{{Method: exact("")}},
						Route: []*networking.HTTPRouteDestination{destination("v2", 0)},
					},
					{
						Name:  "default-v1",
						Route: []*networking.HTTPRouteDestination{destination("v1", 0)},
					},
				},
			},
			want: routeConfiguration(routeToMethod("", nil, clusterNameFor("v1"))),
		},
		{
			name: "a rule without destination is skipped",
			vs: &networking.VirtualService{
				Http: []*networking.HTTPRoute{
					{Name: "no-destination", Match: []*networking.HTTPMatchRequest{{Method: exact("getUser")}}},
					{
						Name:  "default-v1",
						Route: []*networking.HTTPRouteDestination{destination("v1", 0)},
					},
				},
			},
			want: routeConfiguration(routeToMethod("", nil, clusterNameFor("v1"))),
		},
		{
			name: "weights which do not sum to 100 are passed through unchanged",
			vs: &networking.VirtualService{
				Http: []*networking.HTTPRoute{
					{
						Name:  "thrift-traffic-splitting",
						Route: []*networking.HTTPRouteDestination{destination("v1", 20), destination("v2", 70)},
					},
				},
			},
			// The thrift WeightedCluster has no total_weight field, so nothing is normalized.
			want: routeConfiguration(&thrift.Route{
				Match: matchAllMethods(),
				Route: &thrift.RouteAction{
					ClusterSpecifier: &thrift.RouteAction_WeightedClusters{
						WeightedClusters: &thrift.WeightedCluster{
							Clusters: []*thrift.WeightedCluster_ClusterWeight{
								{Name: clusterNameFor("v1"), Weight: &wrappers.UInt32Value{Value: 20}},
								{Name: clusterNameFor("v2"), Weight: &wrappers.UInt32Value{Value: 70}},
							},
						},
					},
				},
			}),
		},
		{
			name: "a virtual service without any rule still yields a default route",
			vs:   &networking.VirtualService{},
			want: routeConfiguration(routeToMethod("", nil, clusterNameFor(""))),
		},
		{
			name: "an empty match element matches all methods",
			vs: &networking.VirtualService{
				Http: []*networking.HTTPRoute{
					{
						Name:  "empty-match",
						Match: []*networking.HTTPMatchRequest{{}},
						Route: []*networking.HTTPRouteDestination{destination("v1", 0)},
					},
				},
			},
			want: routeConfiguration(routeToMethod("", nil, clusterNameFor("v1"))),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildOutboundRouteConfig(newContext(tc.vs))
			if diff := cmp.Diff(tc.want, got, protocmp.Transform()); diff != "" {
				t.Errorf("buildOutboundRouteConfig() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestBuildOutboundRouteConfigIsByteIdenticalForRulesWithoutMatch pins the compatibility guarantee: a VirtualService
// which does not use match must produce exactly the same bytes as the implementation before match translation.
func TestBuildOutboundRouteConfigIsByteIdenticalForRulesWithoutMatch(t *testing.T) {
	testCases := []struct {
		name string
		vs   *networking.VirtualService
	}{
		{name: "no virtual service"},
		{
			name: "single rule",
			vs: &networking.VirtualService{Http: []*networking.HTTPRoute{
				{Route: []*networking.HTTPRouteDestination{destination("v1", 0)}},
			}},
		},
		{
			name: "single rule without subset",
			vs: &networking.VirtualService{Http: []*networking.HTTPRoute{
				{Route: []*networking.HTTPRouteDestination{destination("", 0)}},
			}},
		},
		{
			name: "weighted destinations",
			vs: &networking.VirtualService{Http: []*networking.HTTPRoute{
				{Route: []*networking.HTTPRouteDestination{destination("v1", 20), destination("v2", 80)}},
			}},
		},
		{
			name: "several rules",
			vs: &networking.VirtualService{Http: []*networking.HTTPRoute{
				{Route: []*networking.HTTPRouteDestination{destination("v1", 0)}},
				{Route: []*networking.HTTPRouteDestination{destination("v2", 0)}},
			}},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			context := newContext(tc.vs)
			want := legacyBuildOutboundRouteConfig(context)
			got := buildOutboundRouteConfig(context)

			wantBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(want)
			if err != nil {
				t.Fatalf("failed to marshal the expected route configuration: %v", err)
			}
			gotBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(got)
			if err != nil {
				t.Fatalf("failed to marshal the actual route configuration: %v", err)
			}
			if string(wantBytes) != string(gotBytes) {
				t.Errorf("the generated route configuration changed for a VirtualService without match:\nwant %x\ngot  %x",
					wantBytes, gotBytes)
			}
		})
	}
}

func TestBuildInboundRouteConfig(t *testing.T) {
	clusterName := model.BuildClusterName(model.TrafficDirectionInbound, "", testHost, testPort)
	want := &thrift.RouteConfiguration{
		Name: clusterName,
		Routes: []*thrift.Route{
			routeToMethod("", nil, clusterName),
		},
	}
	got := buildInboundRouteConfig(newContext(nil))
	if diff := cmp.Diff(want, got, protocmp.Transform()); diff != "" {
		t.Errorf("buildInboundRouteConfig() mismatch (-want +got):\n%s", diff)
	}
}

func destination(subset string, weight int32) *networking.HTTPRouteDestination {
	return &networking.HTTPRouteDestination{
		Destination: &networking.Destination{Host: testHost, Subset: subset},
		Weight:      weight,
	}
}

func exact(value string) *networking.StringMatch {
	return &networking.StringMatch{MatchType: &networking.StringMatch_Exact{Exact: value}}
}

func prefix(value string) *networking.StringMatch {
	return &networking.StringMatch{MatchType: &networking.StringMatch_Prefix{Prefix: value}}
}

func regex(value string) *networking.StringMatch {
	return &networking.StringMatch{MatchType: &networking.StringMatch_Regex{Regex: value}}
}

// The helpers below are a verbatim copy of the thrift route builder before the match translation was introduced. They
// are only used to prove that the output stays byte for byte identical for VirtualServices without match.

func legacyBuildOutboundRouteConfig(context *model.EnvoyFilterContext) *thrift.RouteConfiguration {
	var route []*thrift.Route
	clusterName := model.BuildClusterName(model.TrafficDirectionOutbound, "",
		context.ServiceEntry.Spec.Hosts[0], int(context.ServiceEntry.Spec.Ports[0].Number))

	if context.VirtualService == nil {
		route = []*thrift.Route{legacyDefaultRoute(clusterName)}
	} else {
		route = legacyBuildRoute(context)
	}

	return &thrift.RouteConfiguration{
		Name:   clusterName,
		Routes: route,
	}
}

func legacyDefaultRoute(clusterName string) *thrift.Route {
	return &thrift.Route{
		Match: &thrift.RouteMatch{
			MatchSpecifier: &thrift.RouteMatch_MethodName{
				MethodName: "",
			},
		},
		Route: &thrift.RouteAction{
			ClusterSpecifier: &thrift.RouteAction_Cluster{
				Cluster: clusterName,
			},
		},
	}
}

func legacyBuildRoute(context *model.EnvoyFilterContext) []*thrift.Route {
	service := context.ServiceEntry.Spec
	vs := context.VirtualService.Spec

	routes := make([]*thrift.Route, 0)
	for _, http := range vs.Http {
		var routeAction *thrift.RouteAction

		if len(http.Route) > 1 {
			routeAction = legacyBuildWeightedCluster(http, service)
		} else {
			routeAction = legacyBuildSingleCluster(http, service)
		}

		routes = append(routes, &thrift.Route{
			Match: &thrift.RouteMatch{
				MatchSpecifier: &thrift.RouteMatch_MethodName{
					MethodName: "",
				},
			},
			Route: routeAction,
		})
	}
	return routes
}

func legacyBuildSingleCluster(http *networking.HTTPRoute, service *networking.ServiceEntry) *thrift.RouteAction {
	clusterName := model.BuildClusterName(model.TrafficDirectionOutbound, http.Route[0].Destination.Subset,
		service.Hosts[0], int(service.Ports[0].Number))
	return &thrift.RouteAction{
		ClusterSpecifier: &thrift.RouteAction_Cluster{
			Cluster: clusterName,
		},
	}
}

func legacyBuildWeightedCluster(http *networking.HTTPRoute, service *networking.ServiceEntry) *thrift.RouteAction {
	var clusterWeights []*thrift.WeightedCluster_ClusterWeight
	var totalWeight uint32

	for _, route := range http.Route {
		clusterName := model.BuildClusterName(model.TrafficDirectionOutbound, route.Destination.Subset,
			service.Hosts[0], int(service.Ports[0].Number))
		clusterWeight := &thrift.WeightedCluster_ClusterWeight{
			Name:   clusterName,
			Weight: &wrappers.UInt32Value{Value: uint32(route.Weight)}, //nolint:gosec
		}
		clusterWeights = append(clusterWeights, clusterWeight)
		totalWeight += uint32(route.Weight) //nolint:gosec
	}

	return &thrift.RouteAction{
		ClusterSpecifier: &thrift.RouteAction_WeightedClusters{
			WeightedClusters: &thrift.WeightedCluster{
				Clusters: clusterWeights,
			},
		},
	}
}
