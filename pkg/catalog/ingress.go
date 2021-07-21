package catalog

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/pkg/errors"
	networkingV1 "k8s.io/api/networking/v1"
	networkingV1beta1 "k8s.io/api/networking/v1beta1"

	"github.com/openservicemesh/osm/pkg/constants"
	"github.com/openservicemesh/osm/pkg/identity"
	"github.com/openservicemesh/osm/pkg/service"
	"github.com/openservicemesh/osm/pkg/trafficpolicy"
)

const (
	// prefixMatchPathElementsRegex is the regex pattern used to match zero or one grouping of path elements.
	// A path element is of the form /p, /p1/p2, /p1/p2/p3 etc.
	// This regex pattern is used to match paths in a way that is compatible with Kubernetes ingress requirements
	// for Prefix path type, where the prefix must be an element wise prefix and not a string prefix.
	// Ref: https://kubernetes.io/docs/concepts/services-networking/ingress/#path-types
	// It is used to regex match paths such that request /foo matches /foo and /foo/bar, but not /foobar.
	prefixMatchPathElementsRegex = `(/.*)?$`

	// commonRegexChars is a string comprising of characters commonly used in a regex
	// It is used to guess whether a path specified appears as a regex.
	// It is used as a fallback to match ingress paths whose PathType is set to be ImplementationSpecific.
	commonRegexChars = `^$*+[]%|`
)

// Ensure the regex pattern for prefix matching for path elements compiles
var _ = regexp.MustCompile(prefixMatchPathElementsRegex)

// GetIngressTrafficPolicy returns the ingress traffic policy for the given mesh service
// Depending on if the IngressBackend API is enabled, the policies will be generated either from the IngressBackend
// or Kubernetes Ingress API.
func (mc *MeshCatalog) GetIngressTrafficPolicy(svc service.MeshService) (*trafficpolicy.IngressTrafficPolicy, error) {
	if mc.configurator.GetFeatureFlags().EnableIngressBackendPolicy {
		return mc.getIngressTrafficPolicy(svc)
	}

	return mc.getIngressTrafficPolicyFromK8s(svc)
}

// getIngressTrafficPolicy returns the ingress traffic policy for the given mesh service from corresponding IngressBackend resource
func (mc *MeshCatalog) getIngressTrafficPolicy(svc service.MeshService) (*trafficpolicy.IngressTrafficPolicy, error) {
	// TODO(#3779): build policy from IngressBackend
	return nil, nil
}

// getIngressTrafficPolicyFromK8s returns the ingress traffic policy for the given mesh service from the corresponding k8s Ingress resource
// TODO: DEPRECATE once IngressBackend API is the default for configuring an ingress backend.
func (mc *MeshCatalog) getIngressTrafficPolicyFromK8s(svc service.MeshService) (*trafficpolicy.IngressTrafficPolicy, error) {
	httpRoutePolicies, err := mc.getIngressPoliciesFromK8s(svc)
	if err != nil {
		return nil, errors.Wrapf(err, "Error retrieving ingress HTTP routing policies for service %s from Kubernetes", svc)
	}

	if httpRoutePolicies == nil {
		// There are no routes for ingress, which implies ingress does not need to be configured
		return nil, nil
	}

	protocolToPortMap, err := mc.GetTargetPortToProtocolMappingForService(svc)
	if err != nil {
		return nil, errors.Wrapf(err, "Error retrieving port to protocol mapping for service %s", svc)
	}

	enableHTTPSIngress := mc.configurator.UseHTTPSIngress()
	var trafficMatches []*trafficpolicy.IngressTrafficMatch
	// Create protocol specific ingress filter chains per port to handle different ports serving different protocols
	for port, appProtocol := range protocolToPortMap {
		if appProtocol != constants.ProtocolHTTP {
			// Only HTTP ports can accept traffic using k8s Ingress
			continue
		}

		trafficMatch := &trafficpolicy.IngressTrafficMatch{
			Port: port,
		}

		if enableHTTPSIngress {
			// Configure 2 taffic matches for HTTPS ingress (TLS):
			// 1. Without SNI: to match clients that don't set the SNI
			// 2. With SNI: to match clients that set the SNI

			trafficMatch.Name = fmt.Sprintf("ingress_%s_%d_%s", svc, port, constants.ProtocolHTTPS)
			trafficMatch.Protocol = constants.ProtocolHTTPS
			trafficMatch.SkipClientCertValidation = true
			trafficMatches = append(trafficMatches, trafficMatch)

			trafficMatchWithSNI := *trafficMatch
			trafficMatchWithSNI.Name = fmt.Sprintf("ingress_%s_%d_%s_with_sni", svc, port, constants.ProtocolHTTPS)
			trafficMatchWithSNI.ServerNames = []string{svc.ServerName()}
			trafficMatches = append(trafficMatches, &trafficMatchWithSNI)
		} else {
			trafficMatch.Name = fmt.Sprintf("ingress_%s_%d_%s", svc, port, constants.ProtocolHTTP)
			trafficMatch.Protocol = constants.ProtocolHTTP
			trafficMatches = append(trafficMatches, trafficMatch)
		}
	}

	return &trafficpolicy.IngressTrafficPolicy{
		TrafficMatches:    trafficMatches,
		HTTPRoutePolicies: httpRoutePolicies,
	}, nil
}

// getIngressPoliciesFromK8s returns a list of inbound traffic policies for a service as defined in observed ingress k8s resources.
func (mc *MeshCatalog) getIngressPoliciesFromK8s(svc service.MeshService) ([]*trafficpolicy.InboundTrafficPolicy, error) {
	var inboundTrafficPolicies []*trafficpolicy.InboundTrafficPolicy

	// Build policies for ingress v1
	if v1Policies, err := mc.getIngressPoliciesNetworkingV1(svc); err != nil {
		log.Error().Err(err).Msgf("Error building inbound ingress v1 inbound policies for service %s", svc)
	} else {
		inboundTrafficPolicies = append(inboundTrafficPolicies, v1Policies...)
	}

	// Build policies for ingress v1beta1
	if v1beta1Policies, err := mc.getIngressPoliciesNetworkingV1beta1(svc); err != nil {
		log.Error().Err(err).Msgf("Error building inbound ingress v1beta inbound policies for service %s", svc)
	} else {
		inboundTrafficPolicies = append(inboundTrafficPolicies, v1beta1Policies...)
	}

	return inboundTrafficPolicies, nil
}

func getIngressTrafficPolicyName(name, namespace, host string) string {
	policyName := fmt.Sprintf("%s.%s|%s", name, namespace, host)
	return policyName
}

// getIngressPoliciesNetworkingV1beta1 returns the list of inbound traffic policies associated with networking.k8s.io/v1beta1 ingress resources for the given service
func (mc *MeshCatalog) getIngressPoliciesNetworkingV1beta1(svc service.MeshService) ([]*trafficpolicy.InboundTrafficPolicy, error) {
	var inboundIngressPolicies []*trafficpolicy.InboundTrafficPolicy

	ingresses, err := mc.ingressMonitor.GetIngressNetworkingV1beta1(svc)
	if err != nil {
		log.Error().Err(err).Msgf("Failed to get ingress resources for service %s", svc)
		return inboundIngressPolicies, err
	}
	if len(ingresses) == 0 {
		log.Trace().Msgf("No ingress resources found for service %s", svc)
		return inboundIngressPolicies, err
	}

	ingressWeightedCluster := getDefaultWeightedClusterForService(svc)

	for _, ingress := range ingresses {
		if ingress.Spec.Backend != nil && ingress.Spec.Backend.ServiceName == svc.Name {
			wildcardIngressPolicy := trafficpolicy.NewInboundTrafficPolicy(getIngressTrafficPolicyName(ingress.ObjectMeta.Name, ingress.ObjectMeta.Namespace, constants.WildcardHTTPMethod), []string{constants.WildcardHTTPMethod})
			wildcardIngressPolicy.AddRule(*trafficpolicy.NewRouteWeightedCluster(trafficpolicy.WildCardRouteMatch, []service.WeightedCluster{ingressWeightedCluster}), identity.WildcardServiceIdentity)
			inboundIngressPolicies = trafficpolicy.MergeInboundPolicies(DisallowPartialHostnamesMatch, inboundIngressPolicies, wildcardIngressPolicy)
		}

		for _, rule := range ingress.Spec.Rules {
			domain := rule.Host
			if domain == "" {
				domain = constants.WildcardHTTPMethod
			}
			ingressPolicy := trafficpolicy.NewInboundTrafficPolicy(getIngressTrafficPolicyName(ingress.ObjectMeta.Name, ingress.ObjectMeta.Namespace, domain), []string{domain})

			for _, ingressPath := range rule.HTTP.Paths {
				if ingressPath.Backend.ServiceName != svc.Name {
					continue
				}

				httpRouteMatch := trafficpolicy.HTTPRouteMatch{
					Methods: []string{constants.WildcardHTTPMethod},
				}

				// Default ingress path type to PathTypeImplementationSpecific if unspecified
				pathType := networkingV1beta1.PathTypeImplementationSpecific
				if ingressPath.PathType != nil {
					pathType = *ingressPath.PathType
				}

				switch pathType {
				case networkingV1beta1.PathTypeExact:
					// Exact match
					// Request /foo matches path /foo, not /foobar or /foo/bar
					httpRouteMatch.Path = ingressPath.Path
					httpRouteMatch.PathMatchType = trafficpolicy.PathMatchExact

				case networkingV1beta1.PathTypePrefix:
					// Element wise prefix match
					// Request /foo matches path /foo and /foo/bar, not /foobar
					if ingressPath.Path == "/" {
						// A wildcard path '/' for Prefix pathType must be matched
						// as a string based prefix match, ie. path '/' should
						// match any path in the request.
						httpRouteMatch.Path = ingressPath.Path
						httpRouteMatch.PathMatchType = trafficpolicy.PathMatchPrefix
					} else {
						// Non-wildcard path of the form '/path' must be matched as a
						// regex match to meet k8s Ingress API requirement of element-wise
						// prefix matching.
						// There is also the requirement for prefix /foo/ to match /foo
						// based on k8s API interpretation of element-wise matching, so
						// account for this case by trimming trailing '/'.
						path := strings.TrimRight(ingressPath.Path, "/")
						httpRouteMatch.Path = path + prefixMatchPathElementsRegex
						httpRouteMatch.PathMatchType = trafficpolicy.PathMatchRegex
					}

				case networkingV1beta1.PathTypeImplementationSpecific:
					httpRouteMatch.Path = ingressPath.Path
					// If the path looks like a regex, use regex matching.
					// Else use string based prefix matching.
					if strings.ContainsAny(ingressPath.Path, commonRegexChars) {
						// Path contains regex characters, use regex matching for the path
						// Request /foo/bar matches path /foo.*
						httpRouteMatch.PathMatchType = trafficpolicy.PathMatchRegex
					} else {
						// String based prefix path matching
						// Request /foo matches /foo/bar and /foobar
						httpRouteMatch.PathMatchType = trafficpolicy.PathMatchPrefix
					}

				default:
					log.Error().Msgf("Invalid pathType=%s unspecified for path %s in ingress resource %s/%s, ignoring this path", *ingressPath.PathType, ingressPath.Path, ingress.Namespace, ingress.Name)
					continue
				}

				ingressPolicy.AddRule(*trafficpolicy.NewRouteWeightedCluster(httpRouteMatch, []service.WeightedCluster{ingressWeightedCluster}), identity.WildcardServiceIdentity)
			}

			// Only create an ingress policy if the ingress policy resulted in valid rules
			if len(ingressPolicy.Rules) > 0 {
				inboundIngressPolicies = trafficpolicy.MergeInboundPolicies(DisallowPartialHostnamesMatch, inboundIngressPolicies, ingressPolicy)
			}
		}
	}
	return inboundIngressPolicies, nil
}

// getIngressPoliciesNetworkingV1 returns the list of inbound traffic policies associated with networking.k8s.io/v1 ingress resources for the given service
func (mc *MeshCatalog) getIngressPoliciesNetworkingV1(svc service.MeshService) ([]*trafficpolicy.InboundTrafficPolicy, error) {
	var inboundIngressPolicies []*trafficpolicy.InboundTrafficPolicy

	ingresses, err := mc.ingressMonitor.GetIngressNetworkingV1(svc)
	if err != nil {
		log.Error().Err(err).Msgf("Failed to get ingress resources for service %s", svc)
		return inboundIngressPolicies, err
	}
	if len(ingresses) == 0 {
		log.Trace().Msgf("No ingress resources found for service %s", svc)
		return inboundIngressPolicies, err
	}

	ingressWeightedCluster := getDefaultWeightedClusterForService(svc)

	for _, ingress := range ingresses {
		if ingress.Spec.DefaultBackend != nil && ingress.Spec.DefaultBackend.Service.Name == svc.Name {
			wildcardIngressPolicy := trafficpolicy.NewInboundTrafficPolicy(getIngressTrafficPolicyName(ingress.ObjectMeta.Name, ingress.ObjectMeta.Namespace, constants.WildcardHTTPMethod), []string{constants.WildcardHTTPMethod})
			wildcardIngressPolicy.AddRule(*trafficpolicy.NewRouteWeightedCluster(trafficpolicy.WildCardRouteMatch, []service.WeightedCluster{ingressWeightedCluster}), identity.WildcardServiceIdentity)
			inboundIngressPolicies = trafficpolicy.MergeInboundPolicies(DisallowPartialHostnamesMatch, inboundIngressPolicies, wildcardIngressPolicy)
		}

		for _, rule := range ingress.Spec.Rules {
			domain := rule.Host
			if domain == "" {
				domain = constants.WildcardHTTPMethod
			}
			ingressPolicy := trafficpolicy.NewInboundTrafficPolicy(getIngressTrafficPolicyName(ingress.ObjectMeta.Name, ingress.ObjectMeta.Namespace, domain), []string{domain})

			for _, ingressPath := range rule.HTTP.Paths {
				if ingressPath.Backend.Service.Name != svc.Name {
					continue
				}

				httpRouteMatch := trafficpolicy.HTTPRouteMatch{
					Methods: []string{constants.WildcardHTTPMethod},
				}

				// Default ingress path type to PathTypeImplementationSpecific if unspecified
				pathType := networkingV1.PathTypeImplementationSpecific
				if ingressPath.PathType != nil {
					pathType = *ingressPath.PathType
				}

				switch pathType {
				case networkingV1.PathTypeExact:
					// Exact match
					// Request /foo matches path /foo, not /foobar or /foo/bar
					httpRouteMatch.Path = ingressPath.Path
					httpRouteMatch.PathMatchType = trafficpolicy.PathMatchExact

				case networkingV1.PathTypePrefix:
					// Element wise prefix match
					// Request /foo matches path /foo and /foo/bar, not /foobar
					if ingressPath.Path == "/" {
						// A wildcard path '/' for Prefix pathType must be matched
						// as a string based prefix match, ie. path '/' should
						// match any path in the request.
						httpRouteMatch.Path = ingressPath.Path
						httpRouteMatch.PathMatchType = trafficpolicy.PathMatchPrefix
					} else {
						// Non-wildcard path of the form '/path' must be matched as a
						// regex match to meet k8s Ingress API requirement of element-wise
						// prefix matching.
						// There is also the requirement for prefix /foo/ to match /foo
						// based on k8s API interpretation of element-wise matching, so
						// account for this case by trimming trailing '/'.
						path := strings.TrimRight(ingressPath.Path, "/")
						httpRouteMatch.Path = path + prefixMatchPathElementsRegex
						httpRouteMatch.PathMatchType = trafficpolicy.PathMatchRegex
					}

				case networkingV1.PathTypeImplementationSpecific:
					httpRouteMatch.Path = ingressPath.Path
					// If the path looks like a regex, use regex matching.
					// Else use string based prefix matching.
					if strings.ContainsAny(ingressPath.Path, commonRegexChars) {
						// Path contains regex characters, use regex matching for the path
						// Request /foo/bar matches path /foo.*
						httpRouteMatch.PathMatchType = trafficpolicy.PathMatchRegex
					} else {
						// String based prefix path matching
						// Request /foo matches /foo/bar and /foobar
						httpRouteMatch.PathMatchType = trafficpolicy.PathMatchPrefix
					}

				default:
					log.Error().Msgf("Invalid pathType=%s unspecified for path %s in ingress resource %s/%s, ignoring this path", *ingressPath.PathType, ingressPath.Path, ingress.Namespace, ingress.Name)
					continue
				}

				ingressPolicy.AddRule(*trafficpolicy.NewRouteWeightedCluster(httpRouteMatch, []service.WeightedCluster{ingressWeightedCluster}), identity.WildcardServiceIdentity)
			}

			// Only create an ingress policy if the ingress policy resulted in valid rules
			if len(ingressPolicy.Rules) > 0 {
				inboundIngressPolicies = trafficpolicy.MergeInboundPolicies(DisallowPartialHostnamesMatch, inboundIngressPolicies, ingressPolicy)
			}
		}
	}
	return inboundIngressPolicies, nil
}
