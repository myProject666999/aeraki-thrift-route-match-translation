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
	"fmt"
	"sort"

	routepb "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	thrift "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/thrift_proxy/v3"
	matcher "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	"github.com/golang/protobuf/ptypes/wrappers"
	networking "istio.io/api/networking/v1alpha3"

	"github.com/aeraki-mesh/aeraki/internal/model"
)

// Envoy's thrift_proxy RouteMatch only supports exact method name matching, so a regex engine has to be provided
// explicitly for header matches that use regex.
var regexEngine = &matcher.RegexMatcher_GoogleRe2{GoogleRe2: &matcher.RegexMatcher_GoogleRE2{}}

func buildOutboundRouteConfig(context *model.EnvoyFilterContext) *thrift.RouteConfiguration {
	var route []*thrift.Route
	clusterName := model.BuildClusterName(model.TrafficDirectionOutbound, "",
		context.ServiceEntry.Spec.Hosts[0], int(context.ServiceEntry.Spec.Ports[0].Number))

	if context.VirtualService == nil || context.VirtualService.Spec == nil {
		route = []*thrift.Route{defaultRoute(clusterName)}
	} else {
		route = buildRoute(context)
		if len(route) == 0 {
			// An empty route table would make every request fail, so keep a catch-all route.
			generatorLog.Warnf("no Thrift-expressible route found in VirtualService %s/%s, "+
				"falling back to the default route", context.VirtualService.Namespace, context.VirtualService.Name)
			route = []*thrift.Route{defaultRoute(clusterName)}
		}
	}

	return &thrift.RouteConfiguration{
		Name:   clusterName,
		Routes: route,
	}
}

func buildInboundRouteConfig(context *model.EnvoyFilterContext) *thrift.RouteConfiguration {
	clusterName := model.BuildClusterName(model.TrafficDirectionInbound, "",
		context.ServiceEntry.Spec.Hosts[0], int(context.ServiceEntry.Spec.Ports[0].Number))

	return &thrift.RouteConfiguration{
		Name: clusterName,
		Routes: []*thrift.Route{
			defaultRoute(clusterName),
		},
	}
}

// matchAllMethods builds a RouteMatch which matches any request method name.
func matchAllMethods() *thrift.RouteMatch {
	return &thrift.RouteMatch{
		MatchSpecifier: &thrift.RouteMatch_MethodName{
			MethodName: "", // empty string matches any request method name
		},
	}
}

func defaultRoute(clusterName string) *thrift.Route {
	return &thrift.Route{
		Match: matchAllMethods(),
		Route: &thrift.RouteAction{
			ClusterSpecifier: &thrift.RouteAction_Cluster{
				Cluster: clusterName,
			},
		},
	}
}

// buildRoute converts the HTTP routes of the VirtualService into Thrift routes. Envoy matches the routes in order, so
// the order of the rules is preserved.
func buildRoute(context *model.EnvoyFilterContext) []*thrift.Route {
	service := context.ServiceEntry.Spec
	vs := context.VirtualService.Spec

	routes := make([]*thrift.Route, 0, len(vs.Http))
	for i, http := range vs.Http {
		rule := buildRuleName(http, i)
		routeAction, ok := buildRouteAction(http, service, rule)
		if !ok {
			continue
		}

		// Match elements of a single rule are combined with a logical OR, so each of them becomes its own route.
		for _, routeMatch := range buildRouteMatches(http, rule) {
			routes = append(routes, &thrift.Route{
				Match: routeMatch,
				Route: routeAction,
			})
		}
	}
	return routes
}

// buildRuleName returns a human readable name of a rule, used in logs.
func buildRuleName(http *networking.HTTPRoute, index int) string {
	if http.Name != "" {
		return http.Name
	}
	return fmt.Sprintf("http[%d]", index)
}

func buildRouteAction(http *networking.HTTPRoute, service *networking.ServiceEntry,
	rule string) (*thrift.RouteAction, bool) {
	if !hasValidDestinations(http) {
		generatorLog.Warnf("VirtualService rule %q has no valid destination and is skipped", rule)
		return nil, false
	}

	if len(http.Route) > 1 {
		return buildWeightedCluster(http, service), true
	}
	return buildSingleCluster(http, service), true
}

func hasValidDestinations(http *networking.HTTPRoute) bool {
	if len(http.Route) == 0 {
		return false
	}
	for _, destination := range http.Route {
		if destination == nil || destination.Destination == nil {
			return false
		}
	}
	return true
}

// buildRouteMatches converts the match elements of a rule into Thrift RouteMatches. Elements which cannot be expressed
// in Thrift are dropped, and a rule without any expressible element is dropped as a whole.
func buildRouteMatches(http *networking.HTTPRoute, rule string) []*thrift.RouteMatch {
	if len(http.Match) == 0 {
		return []*thrift.RouteMatch{matchAllMethods()}
	}

	matches := make([]*thrift.RouteMatch, 0, len(http.Match))
	for _, match := range http.Match {
		if routeMatch := buildRouteMatch(match, rule); routeMatch != nil {
			matches = append(matches, routeMatch)
		}
	}

	if len(matches) == 0 {
		generatorLog.Warnf("no Thrift-expressible match in VirtualService rule %q, the rule is skipped", rule)
	}
	return matches
}

func buildRouteMatch(match *networking.HTTPMatchRequest, rule string) *thrift.RouteMatch {
	if match == nil {
		return matchAllMethods()
	}

	var methodName string
	if match.Method != nil {
		if _, ok := match.Method.MatchType.(*networking.StringMatch_Exact); !ok {
			// thrift RouteMatch.method_name only supports exact matching.
			generatorLog.Warnf("method %s match in VirtualService rule %q is not supported by thrift, "+
				"the match element is dropped", stringMatchType(match.Method), rule)
			return nil
		}
		methodName = match.Method.GetExact()
		if methodName == "" {
			// An empty method_name means "match any method" in thrift, which would silently widen this rule and
			// swallow the requests of the rules after it.
			generatorLog.Warnf("method exact match in VirtualService rule %q is empty, "+
				"the match element is dropped", rule)
			return nil
		}
	}

	headerMatchers := make([]*routepb.HeaderMatcher, 0, len(match.Headers))
	for _, name := range sortedHeaderNames(match.Headers) {
		headerMatcher, ok := buildHeaderMatcher(name, match.Headers[name], rule)
		if !ok {
			return nil
		}
		headerMatchers = append(headerMatchers, headerMatcher)
	}

	ignored := httpOnlyFields(match)
	if len(ignored) > 0 {
		generatorLog.Warnf("fields %v in VirtualService rule %q have no meaning for thrift and are ignored", ignored, rule)
	}

	if match.Method == nil && len(headerMatchers) == 0 {
		if len(ignored) > 0 {
			// Never degrade such an element to a match-all route, which would swallow the requests of the other rules.
			generatorLog.Warnf("the match element in VirtualService rule %q only contains HTTP-specific fields, "+
				"the match element is dropped", rule)
			return nil
		}
		// A match element without any condition matches all the requests.
		return matchAllMethods()
	}

	routeMatch := &thrift.RouteMatch{
		MatchSpecifier: &thrift.RouteMatch_MethodName{MethodName: methodName},
	}
	if len(headerMatchers) > 0 {
		routeMatch.Headers = headerMatchers
	}
	return routeMatch
}

// httpOnlyFields returns the HTTP-specific match conditions of an element, which have no equivalent in thrift.
func httpOnlyFields(match *networking.HTTPMatchRequest) []string {
	fields := make([]string, 0, 9)
	if match.Uri != nil {
		fields = append(fields, "uri")
	}
	if match.Scheme != nil {
		fields = append(fields, "scheme")
	}
	if match.Authority != nil {
		fields = append(fields, "authority")
	}
	if match.Port != 0 {
		fields = append(fields, "port")
	}
	if len(match.QueryParams) > 0 {
		fields = append(fields, "queryParams")
	}
	if len(match.SourceLabels) > 0 {
		fields = append(fields, "sourceLabels")
	}
	if match.SourceNamespace != "" {
		fields = append(fields, "sourceNamespace")
	}
	if len(match.WithoutHeaders) > 0 {
		fields = append(fields, "withoutHeaders")
	}
	if match.IgnoreUriCase {
		fields = append(fields, "ignoreUriCase")
	}
	return fields
}

func buildHeaderMatcher(name string, value *networking.StringMatch, rule string) (*routepb.HeaderMatcher, bool) {
	switch value.GetMatchType().(type) {
	case *networking.StringMatch_Exact:
		return &routepb.HeaderMatcher{
			Name: name,
			HeaderMatchSpecifier: &routepb.HeaderMatcher_ExactMatch{
				ExactMatch: value.GetExact(),
			},
		}, true
	case *networking.StringMatch_Prefix:
		return &routepb.HeaderMatcher{
			Name: name,
			HeaderMatchSpecifier: &routepb.HeaderMatcher_PrefixMatch{
				PrefixMatch: value.GetPrefix(),
			},
		}, true
	case *networking.StringMatch_Regex:
		return &routepb.HeaderMatcher{
			Name: name,
			HeaderMatchSpecifier: &routepb.HeaderMatcher_SafeRegexMatch{
				SafeRegexMatch: &matcher.RegexMatcher{
					EngineType: regexEngine,
					Regex:      value.GetRegex(),
				},
			},
		}, true
	default:
		generatorLog.Warnf("header %q in VirtualService rule %q has no supported match type, "+
			"the match element is dropped", name, rule)
		return nil, false
	}
}

func stringMatchType(match *networking.StringMatch) string {
	switch match.GetMatchType().(type) {
	case *networking.StringMatch_Exact:
		return "exact"
	case *networking.StringMatch_Prefix:
		return "prefix"
	case *networking.StringMatch_Regex:
		return "regex"
	default:
		return "unknown"
	}
}

func sortedHeaderNames(headers map[string]*networking.StringMatch) []string {
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func buildSingleCluster(http *networking.HTTPRoute, service *networking.ServiceEntry) *thrift.RouteAction {
	clusterName := model.BuildClusterName(model.TrafficDirectionOutbound, http.Route[0].Destination.Subset,
		service.Hosts[0], int(service.Ports[0].Number))
	return &thrift.RouteAction{
		ClusterSpecifier: &thrift.RouteAction_Cluster{
			Cluster: clusterName,
		},
	}
}

func buildWeightedCluster(http *networking.HTTPRoute, service *networking.ServiceEntry) *thrift.RouteAction {
	clusterWeights := make([]*thrift.WeightedCluster_ClusterWeight, 0, len(http.Route))

	for _, route := range http.Route {
		clusterName := model.BuildClusterName(model.TrafficDirectionOutbound, route.Destination.Subset,
			service.Hosts[0], int(service.Ports[0].Number))
		clusterWeight := &thrift.WeightedCluster_ClusterWeight{
			Name:   clusterName,
			Weight: &wrappers.UInt32Value{Value: uint32(route.Weight)}, //nolint:gosec
		}
		clusterWeights = append(clusterWeights, clusterWeight)
	}

	// Unlike dubbo, the thrift WeightedCluster has no total_weight field: Envoy uses the sum of the weights, so the
	// weights are passed through exactly as they are configured.
	return &thrift.RouteAction{
		ClusterSpecifier: &thrift.RouteAction_WeightedClusters{
			WeightedClusters: &thrift.WeightedCluster{
				Clusters: clusterWeights,
			},
		},
	}
}
