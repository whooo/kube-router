package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cloudnativelabs/kube-router/pkg/metrics"
	"github.com/cloudnativelabs/kube-router/pkg/utils"
	"github.com/moby/ipvs"
	"github.com/vishvananda/netlink"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
)

// sync the ipvs service and server details configured to reflect the desired state of Kubernetes services
// and endpoints as learned from services and endpoints information from the api server
func (nsc *NetworkServicesController) syncIpvsServices(serviceInfoMap serviceInfoMap,
	endpointsInfoMap endpointsInfoMap) error {
	start := time.Now()
	defer func() {
		endTime := time.Since(start)
		if nsc.MetricsEnabled {
			metrics.ControllerIpvsServicesSyncTime.Observe(endTime.Seconds())
		}
		klog.V(1).Infof("sync ipvs services took %v", endTime)
	}()

	var err error
	var syncErrors bool

	// map to track all active IPVS services and servers that are setup during sync of
	// cluster IP, nodeport and external IP services
	activeServiceEndpointMap := make(map[string][]string)

	err = nsc.setupClusterIPServices(serviceInfoMap, endpointsInfoMap, activeServiceEndpointMap)
	if err != nil {
		syncErrors = true
		klog.Errorf("Error setting up IPVS services for service cluster IP's: %s", err.Error())
	}
	err = nsc.setupNodePortServices(serviceInfoMap, endpointsInfoMap, activeServiceEndpointMap)
	if err != nil {
		syncErrors = true
		klog.Errorf("Error setting up IPVS services for service nodeport's: %s", err.Error())
	}
	err = nsc.setupExternalIPServices(serviceInfoMap, endpointsInfoMap, activeServiceEndpointMap)
	if err != nil {
		syncErrors = true
		klog.Errorf("Error setting up IPVS services for service external IP's and load balancer IP's: %s",
			err.Error())
	}
	err = nsc.cleanupStaleVIPs(activeServiceEndpointMap)
	if err != nil {
		syncErrors = true
		klog.Errorf("Error cleaning up stale VIP's configured on the dummy interface: %s", err.Error())
	}
	err = nsc.cleanupStaleIPVSConfig(activeServiceEndpointMap)
	if err != nil {
		syncErrors = true
		klog.Errorf("Error cleaning up stale IPVS services and servers: %s", err.Error())
	}

	nsc.cleanupStaleMetrics(activeServiceEndpointMap)

	err = nsc.syncIpvsFirewall()
	if err != nil {
		syncErrors = true
		klog.Errorf("Error syncing ipvs svc iptables rules to permit traffic to service VIP's: %s", err.Error())
	}
	err = nsc.setupForDSR(serviceInfoMap)
	if err != nil {
		syncErrors = true
		klog.Errorf("Error setting up necessary policy based routing configuration needed for "+
			"direct server return: %s", err.Error())
	}

	if syncErrors {
		klog.V(1).Info("One or more errors encountered during sync of IPVS services and servers " +
			"to desired state")
	} else {
		klog.V(1).Info("IPVS servers and services are synced to desired state")
	}

	return nil
}

func (nsc *NetworkServicesController) setupClusterIPServices(serviceInfoMap serviceInfoMap,
	endpointsInfoMap endpointsInfoMap, activeServiceEndpointMap map[string][]string) error {
	ipvsSvcs, err := nsc.ln.ipvsGetServices()
	if err != nil {
		return errors.New("Failed get list of IPVS services due to: " + err.Error())
	}
	for k, svc := range serviceInfoMap {
		protocol := convertSvcProtoToSysCallProto(svc.protocol)

		endpoints := endpointsInfoMap[k]
		dummyVipInterface, err := nsc.ln.getKubeDummyInterface()
		if err != nil {
			return errors.New("Failed creating dummy interface: " + err.Error())
		}
		// assign cluster IP of the service to the dummy interface so that its routable from the pod's on the node
		err = nsc.ln.ipAddrAdd(dummyVipInterface, svc.clusterIP.String(), true)
		if err != nil {
			continue
		}

		// create IPVS service for the service to be exposed through the cluster ip
		ipvsClusterVipSvc, err := nsc.ln.ipvsAddService(ipvsSvcs, svc.clusterIP, protocol, uint16(svc.port),
			svc.sessionAffinity, svc.sessionAffinityTimeoutSeconds, svc.scheduler, svc.flags)
		if err != nil {
			klog.Errorf("Failed to create ipvs service for cluster ip: %s", err.Error())
			continue
		}
		var clusterServiceID = generateIPPortID(svc.clusterIP.String(), svc.protocol, strconv.Itoa(svc.port))
		activeServiceEndpointMap[clusterServiceID] = make([]string, 0)

		// add IPVS remote server to the IPVS service
		for _, endpoint := range endpoints {
			dst := ipvs.Destination{
				Address:       net.ParseIP(endpoint.ip),
				AddressFamily: syscall.AF_INET,
				Port:          uint16(endpoint.port),
				Weight:        1,
			}
			// Conditions on which to add an endpoint on this node:
			// 1) Service is not a local service
			// 2) Service is a local service, but has no active endpoints on this node
			// 3) Service is a local service, has active endpoints on this node, and this endpoint is one of them
			if svc.local {
				if hasActiveEndpoints(endpoints) && !endpoint.isLocal {
					continue
				}
			}

			err := nsc.ln.ipvsAddServer(ipvsClusterVipSvc, &dst)
			if err != nil {
				klog.Errorf(err.Error())
			} else {
				activeServiceEndpointMap[clusterServiceID] = append(activeServiceEndpointMap[clusterServiceID],
					generateEndpointID(endpoint.ip, strconv.Itoa(endpoint.port)))
			}
		}
	}
	return nil
}

func (nsc *NetworkServicesController) setupNodePortServices(serviceInfoMap serviceInfoMap,
	endpointsInfoMap endpointsInfoMap, activeServiceEndpointMap map[string][]string) error {
	ipvsSvcs, err := nsc.ln.ipvsGetServices()
	if err != nil {
		return errors.New("Failed get list of IPVS services due to: " + err.Error())
	}
	for k, svc := range serviceInfoMap {
		protocol := convertSvcProtoToSysCallProto(svc.protocol)

		if svc.nodePort == 0 {
			// service is not NodePort type
			continue
		}
		endpoints := endpointsInfoMap[k]
		if svc.local && !hasActiveEndpoints(endpoints) {
			klog.V(1).Infof("Skipping setting up NodePort service %s/%s as it does not have active endpoints",
				svc.namespace, svc.name)
			continue
		}

		// create IPVS service for the service to be exposed through the nodeport
		var ipvsNodeportSvcs []*ipvs.Service

		var nodeServiceIds []string

		if nsc.nodeportBindOnAllIP {
			// bind on all interfaces instead
			addrs, err := getAllLocalIPs()

			if err != nil {
				klog.Errorf("Could not get list of system addresses for ipvs services: %s", err.Error())
				continue
			}

			if len(addrs) == 0 {
				klog.Errorf("No IP addresses returned for nodeport service creation!")
				continue
			}

			ipvsNodeportSvcs = make([]*ipvs.Service, len(addrs))
			nodeServiceIds = make([]string, len(addrs))

			for i, addr := range addrs {
				ipvsNodeportSvcs[i], err = nsc.ln.ipvsAddService(ipvsSvcs, addr.IP, protocol, uint16(svc.nodePort),
					svc.sessionAffinity, svc.sessionAffinityTimeoutSeconds, svc.scheduler, svc.flags)
				if err != nil {
					klog.Errorf("Failed to create ipvs service for node port due to: %s", err.Error())
					continue
				}

				nodeServiceIds[i] = generateIPPortID(addr.IP.String(), svc.protocol, strconv.Itoa(svc.nodePort))
				activeServiceEndpointMap[nodeServiceIds[i]] = make([]string, 0)
			}
		} else {
			ipvsNodeportSvcs = make([]*ipvs.Service, 1)
			ipvsNodeportSvcs[0], err = nsc.ln.ipvsAddService(ipvsSvcs, nsc.nodeIP, protocol, uint16(svc.nodePort),
				svc.sessionAffinity, svc.sessionAffinityTimeoutSeconds, svc.scheduler, svc.flags)
			if err != nil {
				klog.Errorf("Failed to create ipvs service for node port due to: %s", err.Error())
				continue
			}

			nodeServiceIds = make([]string, 1)
			nodeServiceIds[0] = generateIPPortID(nsc.nodeIP.String(), svc.protocol, strconv.Itoa(svc.nodePort))
			activeServiceEndpointMap[nodeServiceIds[0]] = make([]string, 0)
		}

		for _, endpoint := range endpoints {
			dst := ipvs.Destination{
				Address:       net.ParseIP(endpoint.ip),
				AddressFamily: syscall.AF_INET,
				Port:          uint16(endpoint.port),
				Weight:        1,
			}
			for i := 0; i < len(ipvsNodeportSvcs); i++ {
				if !svc.local || (svc.local && endpoint.isLocal) {
					err := nsc.ln.ipvsAddServer(ipvsNodeportSvcs[i], &dst)
					if err != nil {
						klog.Errorf(err.Error())
					} else {
						activeServiceEndpointMap[nodeServiceIds[i]] =
							append(activeServiceEndpointMap[nodeServiceIds[i]],
								generateEndpointID(endpoint.ip, strconv.Itoa(endpoint.port)))
					}
				}
			}
		}
	}
	return nil
}

func (nsc *NetworkServicesController) setupExternalIPServices(serviceInfoMap serviceInfoMap,
	endpointsInfoMap endpointsInfoMap, activeServiceEndpointMap map[string][]string) error {
	for k, svc := range serviceInfoMap {
		endpoints := endpointsInfoMap[k]

		extIPSet := sets.NewString(svc.externalIPs...)
		if !svc.skipLbIps {
			extIPSet = extIPSet.Union(sets.NewString(svc.loadBalancerIPs...))
		}

		if extIPSet.Len() == 0 {
			// service is not LoadBalancer type and no external IP's are configured
			continue
		}

		if svc.local && !hasActiveEndpoints(endpoints) {
			klog.V(1).Infof("Skipping setting up IPVS service for external IP and LoadBalancer IP "+
				"for the service %s/%s as it does not have active endpoints\n", svc.namespace, svc.name)
			continue
		}
		for _, externalIP := range extIPSet.List() {
			var externalIPServiceID string
			if svc.directServerReturn && svc.directServerReturnMethod == tunnelInterfaceType {
				// for a DSR service, do the work necessary to set up the IPVS service for DSR, then use the FW mark
				// that was generated to add this external IP to the activeServiceEndpointMap
				if err := nsc.setupExternalIPForDSRService(svc, externalIP, endpoints); err != nil {
					return fmt.Errorf("failed to setup DSR endpoint %s: %v", externalIP, err)
				}
				fwMark := nsc.lookupFWMarkByService(externalIP, svc.protocol, fmt.Sprint(svc.port))
				externalIPServiceID = fmt.Sprint(fwMark)
			} else {
				// for a non-DSR service, do the work necessary to setup the IPVS service, then use its IP, protocol,
				// and port to add this external IP to the activeServiceEndpointMap
				if err := nsc.setupExternalIPForService(svc, externalIP, endpoints); err != nil {
					return fmt.Errorf("failed to setup service endpoint %s: %v", externalIP, err)
				}
				externalIPServiceID = generateIPPortID(externalIP, svc.protocol, strconv.Itoa(svc.port))
			}

			// add external service to the activeServiceEndpointMap by its externalIPServiceID. In this case,
			// externalIPServiceID is a little confusing because in the case of DSR services it is the FW Mark that is
			// generated for it, and for non-DSR services it is the combination of: ip + "-" + protocol + "-" + port
			// TODO: remove the difference between DSR and non-DSR services and make a standard
			activeServiceEndpointMap[externalIPServiceID] = make([]string, 0)
			for _, endpoint := range endpoints {
				if !svc.local || (svc.local && endpoint.isLocal) {
					activeServiceEndpointMap[externalIPServiceID] =
						append(activeServiceEndpointMap[externalIPServiceID],
							generateEndpointID(endpoint.ip, strconv.Itoa(endpoint.port)))
				}
			}
		}
	}

	return nil
}

// setupExternalIPForService does the basic work to setup a non-DSR based external IP for service. This includes adding
// the IPVS service to the host if it is missing, and setting up the dummy interface to be able to receive traffic on
// the node.
func (nsc *NetworkServicesController) setupExternalIPForService(svc *serviceInfo, externalIP string,
	endpoints []endpointsInfo) error {
	// Get everything we need to get setup to process the external IP
	protocol := convertSvcProtoToSysCallProto(svc.protocol)
	dummyVipInterface, err := nsc.ln.getKubeDummyInterface()
	if err != nil {
		return fmt.Errorf("failed creating dummy interface: %v", err)
	}
	ipvsSvcs, err := nsc.ln.ipvsGetServices()
	if err != nil {
		return fmt.Errorf("failed get list of IPVS services due to: %v", err)
	}

	// ensure director with vip assigned
	err = nsc.ln.ipAddrAdd(dummyVipInterface, externalIP, true)
	if err != nil && err.Error() != IfaceHasAddr {
		return fmt.Errorf("failed to assign external ip %s to dummy interface %s due to %v",
			externalIP, KubeDummyIf, err)
	}

	// create IPVS service for the service to be exposed through the external ip
	ipvsExternalIPSvc, err := nsc.ln.ipvsAddService(ipvsSvcs, net.ParseIP(externalIP), protocol,
		uint16(svc.port), svc.sessionAffinity, svc.sessionAffinityTimeoutSeconds, svc.scheduler, svc.flags)
	if err != nil {
		return fmt.Errorf("failed to create ipvs service for external ip: %s due to %v",
			externalIP, err)
	}

	// ensure there is NO iptables mangle table rule to FW mark the packet
	fwMark := nsc.lookupFWMarkByService(externalIP, svc.protocol, strconv.Itoa(svc.port))
	switch {
	case fwMark == 0:
		klog.V(2).Infof("no FW mark found for service, nothing to cleanup")
	case fwMark != 0:
		klog.V(2).Infof("the following service '%s:%s:%d' had fwMark associated with it: %d doing "+
			"additional cleanup", externalIP, svc.protocol, svc.port, fwMark)
		if err = nsc.cleanupDSRService(fwMark); err != nil {
			return fmt.Errorf("failed to cleanup DSR service: %v", err)
		}
	}

	// add pod endpoints to the IPVS service
	for _, endpoint := range endpoints {
		// if this specific endpoint isn't local, there is nothing for us to do and we can go to the next record
		if svc.local && !endpoint.isLocal {
			continue
		}

		// create the basic IPVS destination record
		dst := ipvs.Destination{
			Address:       net.ParseIP(endpoint.ip),
			AddressFamily: syscall.AF_INET,
			Port:          uint16(endpoint.port),
			Weight:        1,
		}

		if err = nsc.ln.ipvsAddServer(ipvsExternalIPSvc, &dst); err != nil {
			return fmt.Errorf("unable to add destination %s to externalIP service %s: %v",
				endpoint.ip, externalIP, err)
		}
	}

	return nil
}

// setupExternalIPForDSRService does the basic setup necessary to set up an External IP service for DSR. This includes
// generating a unique FW mark for the service, setting up the mangle rules to apply the FW mark, setting up IPVS to
// work with the FW mark, and ensuring that the IP doesn't exist on the dummy interface so that the traffic doesn't
// accidentally ingress the packet and change it.
//
// For external IPs (which are meant for ingress traffic) configured for DSR, kube-router sets up IPVS services
// based on FWMARK to enable direct server return functionality. DSR requires a director without a VIP
// http://www.austintek.com/LVS/LVS-HOWTO/HOWTO/LVS-HOWTO.routing_to_VIP-less_director.html to avoid martian packets
func (nsc *NetworkServicesController) setupExternalIPForDSRService(svc *serviceInfo, externalIP string,
	endpoints []endpointsInfo) error {
	// Get everything we need to get setup to process the external IP
	protocol := convertSvcProtoToSysCallProto(svc.protocol)
	dummyVipInterface, err := nsc.ln.getKubeDummyInterface()
	if err != nil {
		return errors.New("Failed creating dummy interface: " + err.Error())
	}
	ipvsSvcs, err := nsc.ln.ipvsGetServices()
	if err != nil {
		return errors.New("Failed get list of IPVS services due to: " + err.Error())
	}

	fwMark, err := nsc.generateUniqueFWMark(externalIP, svc.protocol, strconv.Itoa(svc.port))
	if err != nil {
		return fmt.Errorf("failed to generate FW mark")
	}
	ipvsExternalIPSvc, err := nsc.ln.ipvsAddFWMarkService(ipvsSvcs, fwMark, protocol, uint16(svc.port),
		svc.sessionAffinity, svc.sessionAffinityTimeoutSeconds, svc.scheduler, svc.flags)
	if err != nil {
		return fmt.Errorf("failed to create IPVS service for External IP: %s due to: %s",
			externalIP, err.Error())
	}

	externalIPServiceID := fmt.Sprint(fwMark)

	// ensure there is iptables mangle table rule to FWMARK the packet
	err = setupMangleTableRule(externalIP, svc.protocol, strconv.Itoa(svc.port), externalIPServiceID,
		nsc.dsrTCPMSS)
	if err != nil {
		return fmt.Errorf("failed to setup mangle table rule to forward the traffic to external IP")
	}

	// ensure VIP less director. we dont assign VIP to any interface
	err = nsc.ln.ipAddrDel(dummyVipInterface, externalIP)
	if err != nil && err.Error() != IfaceHasNoAddr {
		return fmt.Errorf("failed to delete external ip address from dummyVipInterface due to %v", err)
	}

	// do policy routing to deliver the packet locally so that IPVS can pick the packet
	err = routeVIPTrafficToDirector("0x" + fmt.Sprintf("%x", fwMark))
	if err != nil {
		return fmt.Errorf("failed to setup ip rule to lookup traffic to external IP: %s through custom "+
			"route table due to %v", externalIP, err)
	}

	// add pod endpoints to the IPVS service
	for _, endpoint := range endpoints {
		// if this specific endpoint isn't local, there is nothing for us to do and we can go to the next record
		if svc.local && !endpoint.isLocal {
			continue
		}

		// create the basic IPVS destination record
		dst := ipvs.Destination{
			Address:         net.ParseIP(endpoint.ip),
			AddressFamily:   syscall.AF_INET,
			ConnectionFlags: ipvs.ConnectionFlagTunnel,
			Port:            uint16(endpoint.port),
			Weight:          1,
		}

		// add the destination for the IPVS service for this external IP
		if err = nsc.ln.ipvsAddServer(ipvsExternalIPSvc, &dst); err != nil {
			return fmt.Errorf("unable to add destination %s to externalIP service %s: %v",
				endpoint.ip, externalIP, err)
		}

		// add the external IP to a virtual interface inside the pod so that the pod can receive it
		if err = nsc.addDSRIPInsidePodNetNamespace(externalIP, endpoint.ip); err != nil {
			return fmt.Errorf("unable to setup DSR receiver inside pod: %v", err)
		}
	}

	return nil
}

func (nsc *NetworkServicesController) setupForDSR(serviceInfoMap serviceInfoMap) error {
	klog.V(1).Infof("Setting up policy routing required for Direct Server Return functionality.")
	err := nsc.ln.setupPolicyRoutingForDSR()
	if err != nil {
		return errors.New("Failed setup PBR for DSR due to: " + err.Error())
	}
	klog.V(1).Infof("Custom routing table %s required for Direct Server Return is setup as expected.",
		customDSRRouteTableName)

	klog.V(1).Infof("Setting up custom route table required to add routes for external IP's.")
	err = nsc.ln.setupRoutesForExternalIPForDSR(serviceInfoMap)
	if err != nil {
		klog.Errorf("failed setup custom routing table required to add routes for external IP's due to: %v",
			err)
		return fmt.Errorf("failed setup custom routing table required to add routes for external IP's due to: %v",
			err)
	}
	klog.V(1).Infof("Custom routing table required for Direct Server Return (%s) is setup as expected.",
		externalIPRouteTableName)
	return nil
}

func (nsc *NetworkServicesController) cleanupStaleVIPs(activeServiceEndpointMap map[string][]string) error {
	// cleanup stale IPs on dummy interface
	klog.V(1).Info("Cleaning up if any, old service IPs on dummy interface")
	// This represents "ip - protocol - port" that is created as the key to activeServiceEndpointMap in
	// generateIPPortID()
	const expectedServiceIDParts = 3
	addrActive := make(map[string]bool)
	for k := range activeServiceEndpointMap {
		// verify active and its a generateIPPortID() type service
		if strings.Contains(k, "-") {
			parts := strings.SplitN(k, "-", expectedServiceIDParts)
			addrActive[parts[0]] = true
		}
	}

	dummyVipInterface, err := nsc.ln.getKubeDummyInterface()
	if err != nil {
		return errors.New("Failed creating dummy interface: " + err.Error())
	}
	var addrs []netlink.Addr
	addrs, err = netlink.AddrList(dummyVipInterface, netlink.FAMILY_V4)
	if err != nil {
		return errors.New("Failed to list dummy interface IPs: " + err.Error())
	}
	for _, addr := range addrs {
		isActive := addrActive[addr.IP.String()]
		if !isActive {
			klog.V(1).Infof("Found an IP %s which is no longer needed so cleaning up", addr.IP.String())
			err := nsc.ln.ipAddrDel(dummyVipInterface, addr.IP.String())
			if err != nil {
				klog.Errorf("Failed to delete stale IP %s due to: %s",
					addr.IP.String(), err.Error())
				continue
			}
		}
	}
	return nil
}

func (nsc *NetworkServicesController) cleanupStaleIPVSConfig(activeServiceEndpointMap map[string][]string) error {
	ipvsSvcs, err := nsc.ln.ipvsGetServices()
	if err != nil {
		return errors.New("failed get list of IPVS services due to: " + err.Error())
	}

	// cleanup stale ipvs service and servers
	klog.V(1).Info("Cleaning up if any, old ipvs service and servers which are no longer needed")

	var protocol string
	for _, ipvsSvc := range ipvsSvcs {
		// Note that this isn't all that safe of an assumption because FWMark services have a completely different
		// protocol. So do SCTP services. However, we don't deal with SCTP in kube-router and FWMark is handled below.
		protocol = convertSysCallProtoToSvcProto(ipvsSvc.Protocol)
		// FWMark services by definition don't have a protocol, so we exclude those from the conditional so that they
		// can be cleaned up correctly.
		if protocol == noneProtocol && ipvsSvc.FWMark == 0 {
			klog.Warningf("failed to convert protocol %d to a valid IPVS protocol for service: %s skipping",
				ipvsSvc.Protocol, ipvsSvc.Address.String())
			continue
		}

		var key string
		switch {
		case ipvsSvc.Address != nil:
			key = generateIPPortID(ipvsSvc.Address.String(), protocol, strconv.Itoa(int(ipvsSvc.Port)))
		case ipvsSvc.FWMark != 0:
			key = fmt.Sprint(ipvsSvc.FWMark)
		default:
			continue
		}

		endpointIDs, ok := activeServiceEndpointMap[key]
		// Only delete the service if it's not there anymore to prevent flapping
		// old: if !ok || len(endpointIDs) == 0 {
		if !ok {
			excluded := false
			for _, excludedCidr := range nsc.excludedCidrs {
				if excludedCidr.Contains(ipvsSvc.Address) {
					excluded = true
					break
				}
			}

			if excluded {
				klog.V(1).Infof("Ignoring deletion of an IPVS service %s in an excluded cidr",
					ipvsServiceString(ipvsSvc))
				continue
			}

			klog.V(1).Infof("Found a IPVS service %s which is no longer needed so cleaning up",
				ipvsServiceString(ipvsSvc))
			if ipvsSvc.FWMark != 0 {
				_, _, _, err = nsc.lookupServiceByFWMark(ipvsSvc.FWMark)
				if err != nil {
					klog.V(1).Infof("no FW mark found for service, nothing to cleanup: %v", err)
				} else if err = nsc.cleanupDSRService(ipvsSvc.FWMark); err != nil {
					klog.Errorf("failed to cleanup DSR service: %v", err)
				}
			}
			err = nsc.ln.ipvsDelService(ipvsSvc)
			if err != nil {
				klog.Errorf("Failed to delete stale IPVS service %s due to: %s",
					ipvsServiceString(ipvsSvc), err.Error())
				continue
			}
		} else {
			dsts, err := nsc.ln.ipvsGetDestinations(ipvsSvc)
			if err != nil {
				klog.Errorf("Failed to get list of servers from ipvs service")
			}
			for _, dst := range dsts {
				validEp := false
				for _, epID := range endpointIDs {
					if epID == generateEndpointID(dst.Address.String(), strconv.Itoa(int(dst.Port))) {
						validEp = true
						break
					}
				}
				if !validEp {
					klog.V(1).Infof("Found a destination %s in service %s which is no longer needed so "+
						"cleaning up", ipvsDestinationString(dst), ipvsServiceString(ipvsSvc))
					err = nsc.ipvsDeleteDestination(ipvsSvc, dst)
					if err != nil {
						klog.Errorf("Failed to delete destination %s from ipvs service %s",
							ipvsDestinationString(dst), ipvsServiceString(ipvsSvc))
					}
				}
			}
		}
	}
	return nil
}

// cleanupDSRService takes an FW mark was its only input and uses that to lookup the service and then remove DSR
// specific pieces of that service that may be left-over from the service provisioning.
func (nsc *NetworkServicesController) cleanupDSRService(fwMark uint32) error {
	ipAddress, proto, port, err := nsc.lookupServiceByFWMark(fwMark)
	if err != nil {
		return fmt.Errorf("no service was found for FW mark: %d, service may not be all the way cleaned up: %v",
			fwMark, err)
	}

	// cleanup mangle rules
	klog.V(2).Infof("service %s:%s:%d was found, continuing with DSR service cleanup", ipAddress, proto, port)
	mangleTableRulesDump := bytes.Buffer{}
	var mangleTableRules []string
	if err := utils.SaveInto("mangle", &mangleTableRulesDump); err != nil {
		klog.Errorf("Failed to run iptables-save: %s" + err.Error())
	} else {
		mangleTableRules = strings.Split(mangleTableRulesDump.String(), "\n")
	}

	// All of the iptables-save output here prints FW marks in hexadecimal, if we are doing string searching, our search
	// input needs to be in hex also
	//nolint:gomnd // we're converting to hex here, we don't need to track this as a constant
	fwMarkStr := strconv.FormatInt(int64(fwMark), 16)
	for _, mangleTableRule := range mangleTableRules {
		if strings.Contains(mangleTableRule, ipAddress) && strings.Contains(mangleTableRule, fwMarkStr) {
			klog.V(2).Infof("found mangle rule to cleanup: %s", mangleTableRule)

			// When we cleanup the iptables rule, we need to pass FW mark as an int string rather than a hex string
			err = nsc.ln.cleanupMangleTableRule(ipAddress, proto, strconv.Itoa(port), strconv.Itoa(int(fwMark)),
				nsc.dsrTCPMSS)
			if err != nil {
				klog.Errorf("failed to verify and cleanup any mangle table rule to FORWARD the traffic "+
					"to external IP due to: %v", err)
				continue
			} else {
				// cleanupMangleTableRule will clean all rules in the table, so there is no need to continue looping
				break
			}
		}
	}

	// cleanup the fwMarkMap to ensure that we don't accidentally build state
	delete(nsc.fwMarkMap, fwMark)
	return nil
}

func (nsc *NetworkServicesController) cleanupStaleMetrics(activeServiceEndpointMap map[string][]string) {
	for k, v := range nsc.metricsMap {
		if _, ok := activeServiceEndpointMap[k]; ok {
			continue
		}

		metrics.ServiceBpsIn.DeleteLabelValues(v...)
		metrics.ServiceBpsOut.DeleteLabelValues(v...)
		metrics.ServiceBytesIn.DeleteLabelValues(v...)
		metrics.ServiceBytesOut.DeleteLabelValues(v...)
		metrics.ServiceCPS.DeleteLabelValues(v...)
		metrics.ServicePacketsIn.DeleteLabelValues(v...)
		metrics.ServicePacketsOut.DeleteLabelValues(v...)
		metrics.ServicePpsIn.DeleteLabelValues(v...)
		metrics.ServicePpsOut.DeleteLabelValues(v...)
		metrics.ServiceTotalConn.DeleteLabelValues(v...)
		metrics.ControllerIpvsServices.Dec()
		delete(nsc.metricsMap, k)
	}
}
