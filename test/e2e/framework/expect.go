package framework

import (
	"context"
	"encoding/json"
	"fmt"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrlCfg "k8s.io/cloud-provider-alibaba-cloud/pkg/config"
	"k8s.io/cloud-provider-alibaba-cloud/pkg/controller/helper"
	"k8s.io/cloud-provider-alibaba-cloud/pkg/controller/service"
	"k8s.io/cloud-provider-alibaba-cloud/pkg/model"
	prvd "k8s.io/cloud-provider-alibaba-cloud/pkg/provider"
	"k8s.io/cloud-provider-alibaba-cloud/pkg/provider/alibaba/base"
	"k8s.io/cloud-provider-alibaba-cloud/pkg/provider/alibaba/vpc"
	"k8s.io/cloud-provider-alibaba-cloud/pkg/util"
	"k8s.io/cloud-provider-alibaba-cloud/test/e2e/client"
	"k8s.io/cloud-provider-alibaba-cloud/test/e2e/options"
	"k8s.io/cloud-provider/api"
	"k8s.io/klog/v2"
	"os"
	"strconv"
	"strings"
	"time"
)

func (f *Framework) ExpectLoadBalancerEqual(svc *v1.Service) error {
	reqCtx := &service.RequestContext{
		Service: svc,
		Anno:    service.NewAnnotationRequest(svc),
	}

	var retErr error
	_ = wait.PollImmediate(5*time.Second, 30*time.Second, func() (done bool, err error) {
		// if not find slb, skip retry
		svc, remote, err := f.FindLoadBalancer()
		if err != nil {
			retErr = err
			return false, err
		}

		// check whether the slb and svc is reconciled
		klog.Infof("check whether the slb %s has been synced", remote.LoadBalancerAttribute.LoadBalancerId)
		if err := loadBalancerAttrEqual(f, reqCtx.Anno, svc, remote.LoadBalancerAttribute); err != nil {
			retErr = err
			return false, nil
		}

		if reqCtx.Anno.Get(service.LoadBalancerId) == "" || isOverride(reqCtx.Anno) {
			if err := listenerAttrEqual(reqCtx, remote.Listeners); err != nil {
				retErr = err
				return false, nil
			}
		}

		if err := vsgAttrEqual(f, reqCtx, remote); err != nil {
			retErr = err
			return false, nil
		}

		klog.Infof("slb %s sync successfully", remote.LoadBalancerAttribute.LoadBalancerId)
		retErr = nil
		return true, nil
	})

	return retErr
}

func (f *Framework) ExpectLoadBalancerClean(svc *v1.Service, remote *model.LoadBalancer) error {
	for _, lis := range remote.Listeners {
		if lis.IsUserManaged || lis.NamedKey == nil {
			continue
		}
		if lis.NamedKey.ServiceName == svc.Name &&
			lis.NamedKey.Namespace == svc.Namespace &&
			lis.NamedKey.CID == options.TestConfig.ClusterId {
			return fmt.Errorf("slb %s listener %d is managed by ccm, but do not deleted",
				remote.LoadBalancerAttribute.LoadBalancerId, lis.ListenerPort)
		}
	}

	for _, vg := range remote.VServerGroups {
		if vg.IsUserManaged || vg.NamedKey == nil {
			continue
		}

		if vg.NamedKey.ServiceName == svc.Name &&
			vg.NamedKey.Namespace == svc.Namespace &&
			vg.NamedKey.CID == options.TestConfig.ClusterId {

			hasUserManagedNode := false
			for _, b := range vg.Backends {
				if b.Description != vg.VGroupName {
					hasUserManagedNode = true
				}
			}
			if !hasUserManagedNode {
				return fmt.Errorf("slb %s vgroup %s is managed by ccm, but do not deleted",
					remote.LoadBalancerAttribute.LoadBalancerId, vg.VGroupId)
			}
		}
	}

	return nil
}

func isOverride(anno *service.AnnotationRequest) bool {
	return anno.Get(service.LoadBalancerId) != "" && anno.Get(service.OverrideListener) == "true"
}

func loadBalancerAttrEqual(f *Framework, anno *service.AnnotationRequest, svc *v1.Service, lb model.LoadBalancerAttribute) error {
	if id := anno.Get(service.LoadBalancerId); id != "" {
		if lb.LoadBalancerId != id {
			return fmt.Errorf("expected slb id %s, got %s", id, lb.LoadBalancerId)
		}
	}
	if spec := anno.Get(service.Spec); spec != "" {
		if string(lb.LoadBalancerSpec) != spec {
			return fmt.Errorf("expected slb spec %s, got %s", spec, lb.LoadBalancerSpec)
		}
	}
	if AddressType := anno.Get(service.AddressType); AddressType != "" {
		if string(lb.AddressType) != AddressType {
			return fmt.Errorf("expected slb AddressType %s, got %s", AddressType, lb.AddressType)
		}
	}
	if paymentType := anno.Get(service.ChargeType); paymentType != "" {
		klog.Infof("in, chargeType: svc %s, lb %s", paymentType, lb.InternetChargeType)
		if string(lb.InternetChargeType) != paymentType {
			return fmt.Errorf("expected slb payment %s, got %s", paymentType, lb.InternetChargeType)
		}
		if paymentType == string(model.PayByBandwidth) {
			if Bandwidth := anno.Get(service.Bandwidth); Bandwidth != "" {
				if strconv.Itoa(lb.Bandwidth) != Bandwidth {
					return fmt.Errorf("expected slb Bandwidth %s, got %d", Bandwidth, lb.Bandwidth)
				}
			}
		}
	}
	if LoadBalancerName := anno.Get(service.LoadBalancerName); LoadBalancerName != "" {
		if lb.LoadBalancerName != LoadBalancerName {
			return fmt.Errorf("expected slb name %s, got %s", LoadBalancerName, lb.LoadBalancerName)
		}
	}
	if VSwitchId := anno.Get(service.VswitchId); VSwitchId != "" {
		if lb.VSwitchId != VSwitchId {
			return fmt.Errorf("expected slb VswitchId %s, got %s", VSwitchId, lb.VSwitchId)
		}
	}
	if MasterZoneId := anno.Get(service.MasterZoneID); MasterZoneId != "" {
		if lb.MasterZoneId != MasterZoneId {
			return fmt.Errorf("expected slb MasterZoneId %s, got %s", MasterZoneId, lb.MasterZoneId)
		}
	}
	if SlaveZoneId := anno.Get(service.SlaveZoneID); SlaveZoneId != "" {
		if lb.SlaveZoneId != SlaveZoneId {
			return fmt.Errorf("expected slb SlaveZoneId %s, got%s ", SlaveZoneId, lb.SlaveZoneId)
		}
	}
	if DeleteProtection := anno.Get(service.DeleteProtection); DeleteProtection != "" {
		if string(lb.DeleteProtection) != DeleteProtection {
			return fmt.Errorf("expected slb DeleteProtection %s, got %s", DeleteProtection, lb.DeleteProtection)
		}
	}
	if ModificationProtectionStatus := anno.Get(service.ModificationProtection); ModificationProtectionStatus != "" {
		if string(lb.ModificationProtectionStatus) != ModificationProtectionStatus {
			return fmt.Errorf("expected slb ModificationProtectionStatus %s, got %s",
				ModificationProtectionStatus, lb.ModificationProtectionStatus)
		}
	}
	if ResourceGroupId := anno.Get(service.ResourceGroupId); ResourceGroupId != "" {
		if lb.ResourceGroupId != ResourceGroupId {
			return fmt.Errorf("expected lb ResourceGroupId %s, got %s", ResourceGroupId, lb.ResourceGroupId)
		}
	}
	if AdditionalTags := anno.Get(service.AdditionalTags); AdditionalTags != "" {
		tags, err := f.Client.CloudClient.DescribeTags(context.TODO(), lb.LoadBalancerId)
		if err != nil {
			return err
		}
		if !tagsEqual(AdditionalTags, tags) {
			return fmt.Errorf("expected slb AdditionalTags %s, got %s", AdditionalTags, lb.Tags)
		}
	}
	if IPVersion := anno.Get(service.IPVersion); IPVersion != "" {
		if string(lb.AddressIPVersion) != IPVersion {
			return fmt.Errorf("expected slb IPVersion %s, got %s", IPVersion, lb.AddressIPVersion)
		}
	}
	if networkType := anno.Get(service.SLBNetworkType); networkType != "" {
		if lb.NetworkType != networkType {
			return fmt.Errorf("expected slb networkType %s, got %s", networkType, lb.NetworkType)
		}
	}

	if hostName := anno.Get(service.HostName); hostName != "" {
		if len(svc.Status.LoadBalancer.Ingress) != 1 ||
			svc.Status.LoadBalancer.Ingress[0].Hostname != hostName ||
			svc.Status.LoadBalancer.Ingress[0].IP != "" {
			return fmt.Errorf("svc ingress hostname %v is not equal to hostname %s",
				svc.Status.LoadBalancer.Ingress, hostName)
		}
	}

	if eipAnno := anno.Get(service.ExternalIPType); eipAnno == "eip" {
		eips, err := f.Client.CloudClient.DescribeEipAddresses(context.TODO(), string(vpc.SlbInstance), lb.LoadBalancerId)
		if err != nil {
			return err
		}
		if len(eips) != 1 {
			return fmt.Errorf("lb %s has %d eips", lb.LoadBalancerId, len(eips))
		}

		// hostname annotation takes effect first.
		// if set hostname annotation, ip should be nil
		if anno.Get(service.HostName) == "" &&
			(len(svc.Status.LoadBalancer.Ingress) != 1 ||
				svc.Status.LoadBalancer.Ingress[0].IP != eips[0]) {
			return fmt.Errorf("svc ingress ip %v is not equal to eip %s",
				svc.Status.LoadBalancer.Ingress, eips[0])
		}
	}

	if anno.Get(service.ExternalIPType) == "" && anno.Get(service.HostName) == "" {
		if len(svc.Status.LoadBalancer.Ingress) != 1 ||
			svc.Status.LoadBalancer.Ingress[0].IP != lb.Address {
			return fmt.Errorf("svc ingress ip %v is not equal to slb ip %s",
				svc.Status.LoadBalancer.Ingress, lb.Address)
		}
	}
	return nil
}

func tagsEqual(tagSvc string, tagSlb []model.Tag) bool {
	tags := strings.Split(tagSvc, ",")
	for _, m := range tags {
		found := false
		for _, v := range tagSlb {
			if m == fmt.Sprintf("%s=%s", v.TagKey, v.TagValue) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func listenerAttrEqual(reqCtx *service.RequestContext, remote []model.ListenerAttribute) error {
	for _, port := range reqCtx.Service.Spec.Ports {
		proto, err := protocol(reqCtx.Anno.Get(service.ProtocolPort), port)
		if err != nil {
			return err
		}
		find := false
		for _, r := range remote {
			if r.ListenerPort == int(port.Port) && r.Protocol == proto {
				find = true
				switch proto {
				case model.TCP:
					if err := tcpEqual(reqCtx, port, r); err != nil {
						return err
					}

				case model.UDP:
					if err := udpEqual(reqCtx, port, r); err != nil {
						return err
					}
				case model.HTTP:
					if err := httpEqual(reqCtx, port, r); err != nil {
						return err
					}
				case model.HTTPS:
					if err := httpsEqual(reqCtx, port, r); err != nil {
						return err
					}
				default:
					return fmt.Errorf("not support proto: %s", proto)
				}
			}
		}

		if !find {
			return fmt.Errorf("not found listener %d, proto: %s", port.Port, proto)
		}
	}
	return nil
}

func protocol(annotation string, port v1.ServicePort) (string, error) {
	if annotation == "" {
		return strings.ToLower(string(port.Protocol)), nil
	}
	for _, v := range strings.Split(annotation, ",") {
		pp := strings.Split(v, ":")
		if len(pp) < 2 {
			return "", fmt.Errorf("port and "+
				"protocol format must be like 'https:443' with colon separated. got=[%+v]", pp)
		}

		if pp[0] != model.HTTP &&
			pp[0] != model.TCP &&
			pp[0] != model.HTTPS &&
			pp[0] != model.UDP {
			return "", fmt.Errorf("port protocol"+
				" format must be either [http|https|tcp|udp], protocol not supported wit [%s]", pp[0])
		}

		if pp[1] == fmt.Sprintf("%d", port.Port) {
			util.ServiceLog.Info(fmt.Sprintf("port [%d] transform protocol from %s to %s", port.Port, port.Protocol, pp[0]))
			return pp[0], nil
		}
	}
	return strings.ToLower(string(port.Protocol)), nil
}

func genericListenerEqual(reqCtx *service.RequestContext, local v1.ServicePort, remote model.ListenerAttribute) error {
	nameKey := &model.ListenerNamedKey{
		Prefix:      model.DEFAULT_PREFIX,
		CID:         base.CLUSTER_ID,
		Namespace:   reqCtx.Service.Namespace,
		ServiceName: reqCtx.Service.Name,
		Port:        local.Port,
	}
	if remote.Description != nameKey.Key() {
		return fmt.Errorf("expected listener description %s, got %s", nameKey.Key(), remote.Description)
	}

	if aclStatus := reqCtx.Anno.Get(service.AclStatus); aclStatus != "" {
		if string(remote.AclStatus) != aclStatus {
			return fmt.Errorf("expected slb aclStatus %s, got %s", aclStatus, remote.AclStatus)
		}
		if aclStatus == string(model.OnFlag) {
			if aclID := reqCtx.Anno.Get(service.AclID); aclID != "" {
				if remote.AclId != aclID {
					return fmt.Errorf("expected slb aclID %s, got %s", aclID, remote.AclId)
				}
			}
			if aclType := reqCtx.Anno.Get(service.AclType); aclType != "" {
				if remote.AclType != aclType {
					return fmt.Errorf("expected slb aclType %s, got %s", aclType, remote.AclType)
				}
			}
		}
	}
	if scheduler := reqCtx.Anno.Get(service.Scheduler); scheduler != "" {
		if remote.Scheduler != scheduler {
			return fmt.Errorf("expected slb scheduler %s, got %s", scheduler, remote.Scheduler)
		}
	}

	return nil
}

func genericHealthCheckEqual(reqCtx *service.RequestContext, local v1.ServicePort, remote model.ListenerAttribute) error {
	if healthCheckConnectPort := reqCtx.Anno.Get(service.HealthCheckConnectPort); healthCheckConnectPort != "" {
		port, err := strconv.Atoi(healthCheckConnectPort)
		if err != nil {
			return fmt.Errorf("healthCheckConnectPort %s parse error: %s", healthCheckConnectPort, err.Error())
		}
		if remote.HealthCheckConnectPort != port {
			return fmt.Errorf("expected slb healthCheckConnectPort %d, got %d", port, remote.HealthCheckConnectPort)
		}
	}

	if healthCheckInterval := reqCtx.Anno.Get(service.HealthCheckInterval); healthCheckInterval != "" {
		interval, err := strconv.Atoi(healthCheckInterval)
		if err != nil {
			return fmt.Errorf("healthCheckInterval %s parse error: %s", healthCheckInterval, err.Error())
		}
		if remote.HealthCheckInterval != interval {
			return fmt.Errorf("expected slb healthCheckInterval %d, got %d", interval, remote.HealthCheckInterval)
		}
	}

	if healthyThreshold := reqCtx.Anno.Get(service.HealthyThreshold); healthyThreshold != "" {
		threshold, err := strconv.Atoi(healthyThreshold)
		if err != nil {
			return fmt.Errorf("healthyThreshold %s parse error: %s", healthyThreshold, err.Error())
		}
		if remote.HealthyThreshold != threshold {
			return fmt.Errorf("expected slb healthyThreshold %d, got %d", threshold, remote.HealthyThreshold)
		}
	}

	if unhealthyThreshold := reqCtx.Anno.Get(service.UnhealthyThreshold); unhealthyThreshold != "" {
		threshold, err := strconv.Atoi(unhealthyThreshold)
		if err != nil {
			return fmt.Errorf("unhealthyThreshold %s parse error: %s", unhealthyThreshold, err.Error())
		}
		if remote.UnhealthyThreshold != threshold {
			return fmt.Errorf("expected slb unhealthyThreshold %d, got %d", threshold, remote.UnhealthyThreshold)
		}
	}
	return nil
}

func tcpEqual(reqCtx *service.RequestContext, local v1.ServicePort, remote model.ListenerAttribute) error {
	if err := genericListenerEqual(reqCtx, local, remote); err != nil {
		return err
	}
	// health check for tcp is on by default, and cannot be off
	if err := genericHealthCheckEqual(reqCtx, local, remote); err != nil {
		return nil
	}

	if persistenceTimeout := reqCtx.Anno.Get(service.PersistenceTimeout); persistenceTimeout != "" {
		timeout, err := strconv.Atoi(persistenceTimeout)
		if err != nil {
			return fmt.Errorf("persistenceTimeout %s parse error: %s", persistenceTimeout, err.Error())
		}
		if *remote.PersistenceTimeout != timeout {
			return fmt.Errorf("expected slb persistenceTimeout %d, got %d", timeout, remote.PersistenceTimeout)
		}
	}
	if establishedTimeout := reqCtx.Anno.Get(service.EstablishedTimeout); establishedTimeout != "" {
		timeout, err := strconv.Atoi(establishedTimeout)
		if err != nil {
			return fmt.Errorf("establishedTimeout %s parse error: %s", establishedTimeout, err.Error())
		}
		if remote.EstablishedTimeout != timeout {
			return fmt.Errorf("expected slb establishedTimeout %d, got %d", timeout, remote.EstablishedTimeout)
		}
	}
	if healthCheckConnectTimeout := reqCtx.Anno.Get(service.HealthCheckConnectTimeout); healthCheckConnectTimeout != "" {
		timeout, err := strconv.Atoi(healthCheckConnectTimeout)
		if err != nil {
			return fmt.Errorf("healthCheckConnectTimeout %s parse error: %s", healthCheckConnectTimeout, err.Error())
		}
		if remote.HealthCheckConnectTimeout != timeout {
			return fmt.Errorf("expected slb healthCheckConnectTimeout %d, got %d", timeout, remote.HealthCheckConnectTimeout)
		}
	}
	if healthCheckHttpCode := reqCtx.Anno.Get(service.HealthCheckHTTPCode); healthCheckHttpCode != "" {
		if remote.HealthCheckHttpCode != healthCheckHttpCode {
			return fmt.Errorf("expected slb healthCheckHttpCode %s, got %s", healthCheckHttpCode, remote.HealthCheckHttpCode)
		}
	}
	if healthCheckURI := reqCtx.Anno.Get(service.HealthCheckURI); healthCheckURI != "" {
		if remote.HealthCheckURI != healthCheckURI {
			return fmt.Errorf("expected slb healthCheckURI %s, got %s", healthCheckURI, remote.HealthCheckURI)
		}
	}
	if healthCheckType := reqCtx.Anno.Get(service.HealthCheckType); healthCheckType != "" {
		if remote.HealthCheckType != healthCheckType {
			return fmt.Errorf("expected slb healthCheckType %s, got %s", healthCheckType, remote.HealthCheckType)
		}
	}
	if healthCheckDomain := reqCtx.Anno.Get(service.HealthCheckDomain); healthCheckDomain != "" {
		if remote.HealthCheckDomain != healthCheckDomain {
			return fmt.Errorf("expected slb healthCheckDomain %s, got %s", healthCheckDomain, remote.HealthCheckDomain)
		}
	}
	if connectionDrain := reqCtx.Anno.Get(service.ConnectionDrain); connectionDrain != "" {
		if string(remote.ConnectionDrain) != connectionDrain {
			return fmt.Errorf("expected slb connectionDrain %s, got %s", connectionDrain, remote.ConnectionDrain)
		}
	}
	if drainTimeout := reqCtx.Anno.Get(service.ConnectionDrainTimeout); drainTimeout != "" {
		timeout, err := strconv.Atoi(drainTimeout)
		if err != nil {
			return fmt.Errorf("connectionDrainTimeout %s parse error: %s", drainTimeout, err.Error())
		}
		if remote.ConnectionDrainTimeout != timeout {
			return fmt.Errorf("expected slb connectionDrainTimeout %d, got %d", timeout, remote.ConnectionDrainTimeout)
		}
	}
	return nil
}

func udpEqual(reqCtx *service.RequestContext, local v1.ServicePort, remote model.ListenerAttribute) error {
	if err := genericListenerEqual(reqCtx, local, remote); err != nil {
		return err
	}
	// health check for udp is on by default, and cannot be off
	if err := genericHealthCheckEqual(reqCtx, local, remote); err != nil {
		return nil
	}
	if healthCheckConnectTimeout := reqCtx.Anno.Get(service.HealthCheckConnectTimeout); healthCheckConnectTimeout != "" {
		timeout, err := strconv.Atoi(healthCheckConnectTimeout)
		if err != nil {
			return fmt.Errorf("healthCheckConnectTimeout %s parse error: %s", healthCheckConnectTimeout, err.Error())
		}
		if remote.HealthCheckConnectTimeout != timeout {
			return fmt.Errorf("expected slb healthCheckConnectTimeout %d, got %d", timeout, remote.HealthCheckConnectTimeout)
		}
	}
	if connectionDrain := reqCtx.Anno.Get(service.ConnectionDrain); connectionDrain != "" {
		if string(remote.ConnectionDrain) != connectionDrain {
			return fmt.Errorf("expected slb connectionDrain %s, got %s", connectionDrain, remote.ConnectionDrain)
		}
	}
	if drainTimeout := reqCtx.Anno.Get(service.ConnectionDrainTimeout); drainTimeout != "" {
		timeout, err := strconv.Atoi(drainTimeout)
		if err != nil {
			return fmt.Errorf("connectionDrainTimeout %s parse error: %s", drainTimeout, err.Error())
		}
		if remote.ConnectionDrainTimeout != timeout {
			return fmt.Errorf("expected slb connectionDrainTimeout %d, got %d", timeout, remote.ConnectionDrainTimeout)
		}
	}
	return nil
}

func httpEqual(reqCtx *service.RequestContext, local v1.ServicePort, remote model.ListenerAttribute) error {
	if err := genericListenerEqual(reqCtx, local, remote); err != nil {
		return err
	}
	// health check for http is off by default
	if healthCheck := reqCtx.Anno.Get(service.HealthCheckFlag); healthCheck != "" {
		if string(remote.HealthCheck) != healthCheck {
			return fmt.Errorf("expected slb healthCheck %s, got %s", healthCheck, remote.HealthCheck)
		}
		if healthCheck == string(model.OnFlag) {
			if err := genericHealthCheckEqual(reqCtx, local, remote); err != nil {
				return nil
			}
			if healthCheckHttpCode := reqCtx.Anno.Get(service.HealthCheckHTTPCode); healthCheckHttpCode != "" {
				if remote.HealthCheckHttpCode != healthCheckHttpCode {
					return fmt.Errorf("expected slb healthCheckHttpCode %s, got %s", healthCheckHttpCode, remote.HealthCheckHttpCode)
				}
			}
			if healthCheckURI := reqCtx.Anno.Get(service.HealthCheckURI); healthCheckURI != "" {
				if remote.HealthCheckURI != healthCheckURI {
					return fmt.Errorf("expected slb healthCheckURI %s, got %s", healthCheckURI, remote.HealthCheckURI)
				}
			}
			if healthCheckDomain := reqCtx.Anno.Get(service.HealthCheckDomain); healthCheckDomain != "" {
				if remote.HealthCheckDomain != healthCheckDomain {
					return fmt.Errorf("expected slb healthCheckDomain %s, got %s", healthCheckDomain, remote.HealthCheckDomain)
				}
			}
			if healthCheckTimeout := reqCtx.Anno.Get(service.HealthCheckTimeout); healthCheckTimeout != "" {
				timeout, err := strconv.Atoi(healthCheckTimeout)
				if err != nil {
					return fmt.Errorf("healthCheckTimeout %s parse error: %s", healthCheckTimeout, err.Error())
				}
				if remote.HealthCheckTimeout != timeout {
					return fmt.Errorf("expected slb healthCheckTimeout %d, got %d", timeout, remote.HealthCheckTimeout)
				}
			}
			if healthCheckMethod := reqCtx.Anno.Get(service.HealthCheckMethod); healthCheckMethod != "" {
				if remote.HealthCheckMethod != healthCheckMethod {
					return fmt.Errorf("expected slb healthCheckMethod %s, got %s", healthCheckMethod, remote.HealthCheckMethod)
				}
			}
		}
	}

	if stickySession := reqCtx.Anno.Get(service.SessionStick); stickySession != "" {
		if string(remote.StickySession) != stickySession {
			return fmt.Errorf("expected slb stickySession %s, got %s", stickySession, remote.StickySession)
		}
		if stickySession == string(model.OnFlag) {
			if stickySessionType := reqCtx.Anno.Get(service.SessionStickType); stickySessionType != "" {
				if remote.StickySessionType != stickySessionType {
					return fmt.Errorf("expected slb stickySessionType %s, got %s", stickySessionType, remote.StickySessionType)
				}
			}
			if cookie := reqCtx.Anno.Get(service.Cookie); cookie != "" {
				if remote.Cookie != cookie {
					return fmt.Errorf("expected slb cookie %s, got %s", cookie, remote.Cookie)
				}
			}
			if cookieTimeout := reqCtx.Anno.Get(service.CookieTimeout); cookieTimeout != "" {
				timeout, err := strconv.Atoi(cookieTimeout)
				if err != nil {
					return fmt.Errorf("cookieTimeout %s parse error: %s", cookieTimeout, err.Error())
				}
				if remote.CookieTimeout != timeout {
					return fmt.Errorf("expected slb cookieTimeout %d, got %d", timeout, remote.CookieTimeout)
				}
			}
		}

	}

	if xForwardedForProto := reqCtx.Anno.Get(service.XForwardedForProto); xForwardedForProto != "" {
		if string(remote.XForwardedForProto) != xForwardedForProto {
			return fmt.Errorf("expected slb XForwardedForProto %s, got %s", xForwardedForProto, remote.XForwardedForProto)
		}
	}
	if requestTimeout := reqCtx.Anno.Get(service.RequestTimeout); requestTimeout != "" {
		timeout, err := strconv.Atoi(requestTimeout)
		if err != nil {
			return fmt.Errorf("requestTimeout %s parse error: %s", requestTimeout, err.Error())
		}
		if remote.RequestTimeout != timeout {
			return fmt.Errorf("expected slb requestTimeout %d, got %d", timeout, remote.RequestTimeout)
		}
	}
	if idleTimeout := reqCtx.Anno.Get(service.IdleTimeout); idleTimeout != "" {
		timeout, err := strconv.Atoi(idleTimeout)
		if err != nil {
			return fmt.Errorf("idleTimeout %s parse error: %s", idleTimeout, err.Error())
		}
		if remote.IdleTimeout != timeout {
			return fmt.Errorf("expected slb idleTimeout %d, got %d", timeout, remote.IdleTimeout)
		}
	}
	if forwardPort := reqCtx.Anno.Get(service.ForwardPort); forwardPort != "" {
		fPort, err := getForwardPort(forwardPort, int(local.Port))
		if err != nil {
			return fmt.Errorf("forwardPort [%s] parse error: %s", forwardPort, err.Error())
		}
		if remote.ForwardPort != fPort {
			return fmt.Errorf("expected slb forwardPort %d, got %d", fPort, remote.ForwardPort)
		}
		if remote.ListenerForward != model.OnFlag {
			return fmt.Errorf("expected slb listenerForward %s, got %s", model.OnFlag, remote.ListenerForward)
		}
	}
	return nil
}

func httpsEqual(reqCtx *service.RequestContext, local v1.ServicePort, remote model.ListenerAttribute) error {
	if err := genericListenerEqual(reqCtx, local, remote); err != nil {
		return err
	}
	// health check for https is off by default
	if healthCheck := reqCtx.Anno.Get(service.HealthCheckFlag); healthCheck != "" {
		if string(remote.HealthCheck) != healthCheck {
			return fmt.Errorf("expected slb healthCheck %s, got %s", healthCheck, remote.HealthCheck)
		}
		if healthCheck == string(model.OnFlag) {
			if err := genericHealthCheckEqual(reqCtx, local, remote); err != nil {
				return nil
			}
			if healthCheckHttpCode := reqCtx.Anno.Get(service.HealthCheckHTTPCode); healthCheckHttpCode != "" {
				if remote.HealthCheckHttpCode != healthCheckHttpCode {
					return fmt.Errorf("expected slb healthCheckHttpCode %s, got %s", healthCheckHttpCode, remote.HealthCheckHttpCode)
				}
			}
			if healthCheckURI := reqCtx.Anno.Get(service.HealthCheckURI); healthCheckURI != "" {
				if remote.HealthCheckURI != healthCheckURI {
					return fmt.Errorf("expected slb healthCheckURI %s, got %s", healthCheckURI, remote.HealthCheckURI)
				}
			}
			if healthCheckDomain := reqCtx.Anno.Get(service.HealthCheckDomain); healthCheckDomain != "" {
				if remote.HealthCheckDomain != healthCheckDomain {
					return fmt.Errorf("expected slb healthCheckDomain %s, got %s", healthCheckDomain, remote.HealthCheckDomain)
				}
			}
			if healthCheckTimeout := reqCtx.Anno.Get(service.HealthCheckTimeout); healthCheckTimeout != "" {
				timeout, err := strconv.Atoi(healthCheckTimeout)
				if err != nil {
					return fmt.Errorf("healthCheckTimeout %s parse error: %s", healthCheckTimeout, err.Error())
				}
				if remote.HealthCheckTimeout != timeout {
					return fmt.Errorf("expected slb healthCheckTimeout %d, got %d", timeout, remote.HealthCheckTimeout)
				}
			}
			if healthCheckMethod := reqCtx.Anno.Get(service.HealthCheckMethod); healthCheckMethod != "" {
				if remote.HealthCheckMethod != healthCheckMethod {
					return fmt.Errorf("expected slb healthCheckMethod %s, got %s", healthCheckMethod, remote.HealthCheckMethod)
				}
			}
		}
	}

	if stickySession := reqCtx.Anno.Get(service.SessionStick); stickySession != "" {
		if string(remote.StickySession) != stickySession {
			return fmt.Errorf("expected slb stickySession %s, got %s", stickySession, remote.StickySession)
		}

		if stickySession == string(model.OnFlag) {
			if stickySessionType := reqCtx.Anno.Get(service.SessionStickType); stickySessionType != "" {
				if remote.StickySessionType != stickySessionType {
					return fmt.Errorf("expected slb stickySessionType %s, got %s", stickySessionType, remote.StickySessionType)
				}
			}
			if cookie := reqCtx.Anno.Get(service.Cookie); cookie != "" {
				if remote.Cookie != cookie {
					return fmt.Errorf("expected slb cookie %s, got %s", cookie, remote.Cookie)
				}
			}
			if cookieTimeout := reqCtx.Anno.Get(service.CookieTimeout); cookieTimeout != "" {
				timeout, err := strconv.Atoi(cookieTimeout)
				if err != nil {
					return fmt.Errorf("cookieTimeout %s parse error: %s", cookieTimeout, err.Error())
				}
				if remote.CookieTimeout != timeout {
					return fmt.Errorf("expected slb cookieTimeout %d, got %d", timeout, remote.CookieTimeout)
				}
			}
		}
	}

	if idleTimeout := reqCtx.Anno.Get(service.IdleTimeout); idleTimeout != "" {
		timeout, err := strconv.Atoi(idleTimeout)
		if err != nil {
			return fmt.Errorf("idleTimeout %s parse error: %s", idleTimeout, err.Error())
		}
		if remote.IdleTimeout != timeout {
			return fmt.Errorf("expected slb idleTimeout %d, got %d", timeout, remote.IdleTimeout)
		}
	}
	if xForwardedForProto := reqCtx.Anno.Get(service.XForwardedForProto); xForwardedForProto != "" {
		if string(remote.XForwardedForProto) != xForwardedForProto {
			return fmt.Errorf("expected slb XForwardedForProto %s, got %s", xForwardedForProto, remote.XForwardedForProto)
		}
	} else {
		if remote.XForwardedForProto != model.OffFlag {
			return fmt.Errorf("expected slb XForwardedForProto default %s, got %s", model.OffFlag, remote.XForwardedForProto)
		}
	}
	if certId := reqCtx.Anno.Get(service.CertID); certId != "" {
		if remote.CertId != certId {
			return fmt.Errorf("expected slb certId %s, got %s", certId, remote.CertId)
		}
	}
	if enableHttp2 := reqCtx.Anno.Get(service.EnableHttp2); enableHttp2 != "" {
		if string(remote.EnableHttp2) != enableHttp2 {
			return fmt.Errorf("expected slb enableHttp2 %s, got %s", enableHttp2, remote.EnableHttp2)
		}
	}
	if requestTimeout := reqCtx.Anno.Get(service.RequestTimeout); requestTimeout != "" {
		timeout, err := strconv.Atoi(requestTimeout)
		if err != nil {
			return fmt.Errorf("requestTimeout %s parse error: %s", requestTimeout, err.Error())
		}
		if remote.RequestTimeout != timeout {
			return fmt.Errorf("expected slb requestTimeout %d, got %d", timeout, remote.RequestTimeout)
		}
	}
	return nil
}

func getForwardPort(anno string, port int) (int, error) {
	fps := strings.Split(anno, ",")
	for _, fp := range fps {
		p := strings.Split(fp, ":")
		lp, err := strconv.Atoi(p[0])
		if err != nil {
			return 0, fmt.Errorf("parse forward port error: %s", err.Error())
		}
		if lp == port {
			return strconv.Atoi(p[1])
		}
	}
	return 0, fmt.Errorf("cannot find port %d forward port in anno %s", port, anno)
}

func vsgAttrEqual(f *Framework, reqCtx *service.RequestContext, remote *model.LoadBalancer) error {
	for _, port := range reqCtx.Service.Spec.Ports {
		name := getVGroupName(reqCtx.Service, port)
		var (
			vGroupId string
			err      error
		)
		if vGroupAnno := reqCtx.Anno.Get(service.VGroupPort); vGroupAnno != "" {
			vGroupId, err = getVGroupID(reqCtx.Anno.Get(service.VGroupPort), port)
			if err != nil {
				return fmt.Errorf("parse vgroup port annotation %s error: %s", vGroupAnno, err.Error())
			}
		}

		found := false
		for _, vg := range remote.VServerGroups {
			if vg.VGroupName == name {
				found = true
			}
			if vg.VGroupId == vGroupId {
				found = true
				vg.IsUserManaged = true
			}
			if found {
				vg.ServicePort = port
				if isOverride(reqCtx.Anno) && !isUsedByPort(vg, remote.Listeners) {
					return fmt.Errorf("port %d do not use vgroup id: %s", port.Port, vg.VGroupId)
				}
				if weight := reqCtx.Anno.Get(service.VGroupWeight); weight != "" {
					w, err := strconv.Atoi(weight)
					if err != nil {
						return fmt.Errorf("parse weight err")
					}
					vg.VGroupWeight = &w
				}
				equal, err := isBackendEqual(f.Client.KubeClient, reqCtx, vg)
				if err != nil || !equal {
					return fmt.Errorf("port %d and vgroup %s do not have equal backends, error: %v",
						port.Port, vg.VGroupId, err)
				}
				break
			}
		}
		if !found {
			return fmt.Errorf("cannot found vgroup %s", name)
		}
	}
	return nil
}

func getVGroupID(vGroupAnno string, servicePort v1.ServicePort) (string, error) {
	vports := strings.Split(vGroupAnno, ",")
	for _, vport := range vports {
		vp := strings.Split(vport, ":")
		if len(vp) != 2 {
			return "", fmt.Errorf("vgroup-port annotatio format error: %s should be {vgroupid}:{port}", vp)
		}
		if vp[1] == fmt.Sprintf("%d", servicePort.Port) {
			return vp[0], nil
		}
	}
	return "", nil
}

func getVGroupName(svc *v1.Service, servicePort v1.ServicePort) string {
	vGroupPort := ""
	if isENIBackendType(svc) {
		switch servicePort.TargetPort.Type {
		case intstr.Int:
			vGroupPort = fmt.Sprintf("%d", servicePort.TargetPort.IntValue())
		case intstr.String:
			vGroupPort = servicePort.TargetPort.StrVal
		}
	} else {
		vGroupPort = fmt.Sprintf("%d", servicePort.NodePort)
	}
	namedKey := &model.VGroupNamedKey{
		Prefix:      model.DEFAULT_PREFIX,
		Namespace:   svc.Namespace,
		CID:         base.CLUSTER_ID,
		VGroupPort:  vGroupPort,
		ServiceName: svc.Name}
	return namedKey.Key()
}

func isENIBackendType(svc *v1.Service) bool {
	if svc.Annotations[service.BackendType] != "" {
		return svc.Annotations[service.BackendType] == model.ENIBackendType
	}

	if os.Getenv("SERVICE_FORCE_BACKEND_ENI") != "" {
		return os.Getenv("SERVICE_FORCE_BACKEND_ENI") == "true"
	}

	return ctrlCfg.CloudCFG.Global.ServiceBackendType == model.ENIBackendType
}

func isUsedByPort(vg model.VServerGroup, listeners []model.ListenerAttribute) bool {
	for _, l := range listeners {
		if l.ListenerPort == int(vg.ServicePort.Port) {
			return vg.VGroupId == l.VGroupId
		}
	}
	return false
}

func getTrafficPolicy(reqCtx *service.RequestContext) service.TrafficPolicy {
	if isENIBackendType(reqCtx.Service) {
		return service.ENITrafficPolicy
	}
	if reqCtx.Service.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal {
		return service.LocalTrafficPolicy
	}
	return service.ClusterTrafficPolicy
}

func isBackendEqual(client *client.KubeClient, reqCtx *service.RequestContext, vg model.VServerGroup) (bool, error) {
	policy := getTrafficPolicy(reqCtx)
	endpoints, err := client.GetEndpoint()
	if err != nil {
		if !errors.IsNotFound(err) {
			return false, err
		}
		klog.Infof("endpoint is nil")
	}

	nodes, err := client.ListNodes()
	if err != nil {
		return false, err
	}

	var backends []model.BackendAttribute
	switch policy {
	case service.ENITrafficPolicy:
		backends, err = buildENIBackends(reqCtx.Anno, endpoints, vg)
		if err != nil {
			return false, err
		}
	case service.LocalTrafficPolicy:
		backends, err = buildLocalBackends(reqCtx.Anno, endpoints, nodes, vg)
		if err != nil {
			return false, err
		}
	case service.ClusterTrafficPolicy:
		backends, err = buildClusterBackends(reqCtx.Anno, endpoints, nodes, vg)
		if err != nil {
			return false, err
		}
	}
	for _, l := range backends {
		found := false
		for _, r := range vg.Backends {
			if policy == service.ENITrafficPolicy {
				if l.ServerIp == r.ServerIp &&
					l.Port == r.Port &&
					l.Type == model.ENIBackendType {
					if !vg.IsUserManaged && l.Description != r.Description {
						return false, fmt.Errorf("mode %s expected vgroup [%s] backend %s description not equal,"+
							" expect %s, got %s", policy, vg.VGroupId, l.ServerIp, l.Description, r.Description)
					}
					if l.Weight != r.Weight {
						return false, fmt.Errorf("mode %s expected vgroup [%s] backend %s weight not equal,"+
							" expect %d, got %d", policy, vg.VGroupId, l.ServerIp, l.Weight, r.Weight)
					}
					found = true
					break
				}
			} else {
				if l.ServerId == r.ServerId &&
					l.Port == r.Port &&
					l.Type == model.ECSBackendType {
					if !vg.IsUserManaged && l.Description != r.Description {
						return false, fmt.Errorf("mode %s expected vgroup [%s] backend %s description not equal,"+
							" expect %s, got %s", policy, vg.VGroupId, l.ServerIp, l.Description, r.Description)
					}
					if l.Weight != r.Weight {
						return false, fmt.Errorf("mode %s expected vgroup [%s] backend %s weight not equal,"+
							" expect %d, got %d", policy, vg.VGroupId, l.ServerIp, l.Weight, r.Weight)
					}
					found = true
					break
				}
			}
		}
		if !found {
			return false, fmt.Errorf("mode %s expected vgroup [%s] has backend [%+v], got nil, backends [%s]",
				policy, vg.VGroupId, l, vg.BackendInfo())
		}
	}
	return true, nil
}

func buildENIBackends(anno *service.AnnotationRequest, ep *v1.Endpoints, vg model.VServerGroup) ([]model.BackendAttribute, error) {
	var ret []model.BackendAttribute
	for _, subset := range ep.Subsets {
		backendPort := getBackendPort(vg.ServicePort, subset)
		for _, address := range subset.Addresses {
			ret = append(ret, model.BackendAttribute{
				Description: vg.VGroupName,
				ServerIp:    address.IP,
				Port:        backendPort,
				Type:        model.ENIBackendType,
			})
		}
	}
	return setWeightBackends(service.ENITrafficPolicy, ret, vg.VGroupWeight), nil
}

func buildLocalBackends(anno *service.AnnotationRequest, ep *v1.Endpoints, nodes []v1.Node, vg model.VServerGroup) ([]model.BackendAttribute, error) {
	var ret []model.BackendAttribute
	for _, subset := range ep.Subsets {
		for _, addr := range subset.Addresses {
			if addr.NodeName == nil {
				return nil, fmt.Errorf("%s node name is nil", addr.IP)
			}
			node := findNodeByNodeName(nodes, *addr.NodeName)
			if node == nil {
				continue
			}
			if isNodeExcludeFromLoadBalancer(node, anno) {
				continue
			}

			_, id, err := service.NodeFromProviderID(node.Spec.ProviderID)
			if err != nil {
				return nil, fmt.Errorf("providerID %s parse error: %s", node.Spec.ProviderID, err.Error())
			}
			ret = append(ret, model.BackendAttribute{
				Description: vg.VGroupName,
				ServerId:    id,
				Port:        int(vg.ServicePort.NodePort),
				Type:        model.ECSBackendType,
			})
		}
	}

	eciBackends, err := buildECIBackends(ep, nodes, vg)
	if err != nil {
		return nil, fmt.Errorf("build eci backends error: %s", err.Error())
	}

	return setWeightBackends(service.LocalTrafficPolicy, append(ret, eciBackends...), vg.VGroupWeight), nil
}

func buildClusterBackends(anno *service.AnnotationRequest, ep *v1.Endpoints, nodes []v1.Node, vg model.VServerGroup) ([]model.BackendAttribute, error) {
	var ret []model.BackendAttribute
	for _, n := range nodes {
		if isNodeExcludeFromLoadBalancer(&n, anno) {
			continue
		}
		_, id, err := service.NodeFromProviderID(n.Spec.ProviderID)
		if err != nil {
			return nil, fmt.Errorf("providerID %s parse error: %s", n.Spec.ProviderID, err.Error())
		}
		ret = append(ret, model.BackendAttribute{
			Description: vg.VGroupName,
			ServerId:    id,
			Type:        model.ECSBackendType,
			Port:        int(vg.ServicePort.NodePort),
		})
	}

	eciBackends, err := buildECIBackends(ep, nodes, vg)
	if err != nil {
		return nil, fmt.Errorf("build eci backends error: %s", err.Error())
	}
	return setWeightBackends(service.ClusterTrafficPolicy, append(ret, eciBackends...), vg.VGroupWeight), nil
}

func buildECIBackends(ep *v1.Endpoints, nodes []v1.Node, vg model.VServerGroup) ([]model.BackendAttribute, error) {
	var ret []model.BackendAttribute
	for _, subset := range ep.Subsets {
		for _, addr := range subset.Addresses {
			if addr.NodeName == nil {
				return nil, fmt.Errorf("%s node name is nil", addr.IP)
			}
			node := findNodeByNodeName(nodes, *addr.NodeName)
			if node == nil {
				continue
			}
			if isVKNode(*node) {
				backendPort := getBackendPort(vg.ServicePort, subset)
				ret = append(ret, model.BackendAttribute{
					Description: vg.VGroupName,
					ServerIp:    addr.IP,
					Port:        backendPort,
					Type:        model.ENIBackendType,
				})
			}
		}
	}
	return ret, nil
}

func setWeightBackends(mode service.TrafficPolicy, backends []model.BackendAttribute, weight *int) []model.BackendAttribute {
	// use default
	if weight == nil {
		return podNumberAlgorithm(mode, backends)
	}

	return podPercentAlgorithm(mode, backends, *weight)
}

func podNumberAlgorithm(mode service.TrafficPolicy, backends []model.BackendAttribute) []model.BackendAttribute {
	if mode == service.ENITrafficPolicy || mode == service.ClusterTrafficPolicy {
		for i := range backends {
			backends[i].Weight = service.DefaultServerWeight
		}
		return backends
	}

	// LocalTrafficPolicy
	ecsPods := make(map[string]int)
	for _, b := range backends {
		ecsPods[b.ServerId] += 1
	}
	for i := range backends {
		backends[i].Weight = ecsPods[backends[i].ServerId]
	}
	return backends
}

func podPercentAlgorithm(mode service.TrafficPolicy, backends []model.BackendAttribute, weight int) []model.BackendAttribute {
	if weight == 0 {
		for i := range backends {
			backends[i].Weight = 0
		}
		return backends
	}

	if mode == service.ENITrafficPolicy || mode == service.ClusterTrafficPolicy {
		per := weight / len(backends)
		if per < 1 {
			per = 1
		}

		for i := range backends {
			backends[i].Weight = per
		}
		return backends
	}

	// LocalTrafficPolicy
	ecsPods := make(map[string]int)
	for _, b := range backends {
		ecsPods[b.ServerId] += 1
	}
	for i := range backends {
		backends[i].Weight = weight * ecsPods[backends[i].ServerId] / len(backends)
		if backends[i].Weight < 1 {
			backends[i].Weight = 1
		}
	}
	return backends
}

func getBackendPort(port v1.ServicePort, subset v1.EndpointSubset) int {
	if port.TargetPort.Type == intstr.Int {
		return port.TargetPort.IntValue()
	}

	for _, p := range subset.Ports {
		if p.Name == port.Name {
			return int(p.Port)
		}
	}
	return 0
}

func findNodeByNodeName(nodes []v1.Node, nodeName string) *v1.Node {
	for _, n := range nodes {
		if n.Name == nodeName {
			return &n
		}
	}
	klog.Infof("node %s not found ", nodeName)
	return nil
}

func isNodeExcludeFromLoadBalancer(node *v1.Node, anno *service.AnnotationRequest) bool {
	if helper.HasExcludeLabel(node) {
		return true
	}

	if anno.Get(service.BackendLabel) != "" {
		if _, include := node.Labels[anno.Get(service.BackendLabel)]; !include {
			return true
		}
	}

	if node.Spec.Unschedulable && anno.Get(service.RemoveUnscheduled) != "" {
		if anno.Get(service.RemoveUnscheduled) == string(model.OnFlag) {
			return true
		}
	}

	if _, isMaster := node.Labels[service.LabelNodeRoleMaster]; isMaster {
		return true
	}

	if _, exclude := node.Labels[service.LabelNodeExcludeBalancer]; exclude {
		return true
	}

	return false
}

func isVKNode(node v1.Node) bool {
	label, ok := node.Labels["type"]
	return ok && label == service.LabelNodeTypeVK
}

func (f *Framework) ExpectNodeEqual() error {
	var retErr error
	_ = wait.PollImmediate(30*time.Second, 7*time.Minute, func() (done bool, err error) {
		nodes, err := f.Client.KubeClient.ListNodes()
		if err != nil {
			retErr = err
			return false, nil
		}
		var instanceIds []string
		for _, node := range nodes {
			if isVKNode(node) {
				continue
			}
			for _, taint := range node.Spec.Taints {
				if taint.Key == api.TaintExternalCloudProvider {
					retErr = fmt.Errorf("node %s has uninitialized taint", node.Name)
					return false, nil
				}
			}
			for _, condition := range node.Status.Conditions {
				if condition.Type == v1.NodeNetworkUnavailable && condition.Status == v1.ConditionTrue {
					retErr = fmt.Errorf("node %s NetworkUnavailable condition is true", node.Name)
					return false, nil
				}
			}
			instanceIds = append(instanceIds, node.Spec.ProviderID)
		}
		klog.Infof("will check %d instanceIds:%s", len(instanceIds), instanceIds)

		instances, err := f.Client.CloudClient.ListInstances(context.TODO(), instanceIds)
		if err != nil {
			retErr = err
			return false, nil
		}

		for _, node := range nodes {
			cloudTaint := findCloudTaint(node.Spec.Taints)
			if cloudTaint != nil {
				retErr = fmt.Errorf("node %s still has uninitialized taint", node.Name)
				return false, nil
			}
			if isVKNode(node) {
				continue
			}
			_, id, err := service.NodeFromProviderID(node.Spec.ProviderID)
			if err != nil {
				retErr = err
				return false, nil
			}
			found := false
			for _, ins := range instances {
				if ins.InstanceID == id {
					found = true
					if !isNodeAndInsEqual(node, ins) {
						retErr = fmt.Errorf("node %s ip not equals to ecs %s", node.Name, ins.InstanceID)
						return false, nil
					}
				}
			}
			if !found {
				retErr = fmt.Errorf("node %s, provider id %s has not found ecs", node.Name, id)
				return false, nil
			}
		}
		retErr = nil
		return true, nil
	})

	return retErr
}

func findCloudTaint(taints []v1.Taint) *v1.Taint {
	for _, taint := range taints {
		if taint.Key == api.TaintExternalCloudProvider {
			return &taint
		}
	}
	return nil
}

func isNodeAndInsEqual(node v1.Node, ins *prvd.NodeAttribute) bool {
	typeEqual := node.Labels[v1.LabelInstanceType] == ins.InstanceType &&
		node.Labels[v1.LabelInstanceTypeStable] == ins.InstanceType
	if typeEqual == false {
		klog.Errorf("node.Labels[v1.LabelInstanceType]:%s,node.Labels[v1.LabelInstanceTypeStable]:%s, ins.InstanceType:%s",
			node.Labels[v1.LabelInstanceType], node.Labels[v1.LabelInstanceTypeStable], ins.InstanceType)
		return false
	}
	zoneEqual := node.Labels[v1.LabelZoneFailureDomain] == ins.Zone &&
		node.Labels[v1.LabelZoneFailureDomainStable] == ins.Zone
	if zoneEqual == false {
		klog.Errorf("node.Labels[v1.LabelZoneFailureDomain]:%s,node.Labels[v1.LabelZoneFailureDomainStable]:%s, ins.Zone:%s",
			node.Labels[v1.LabelZoneFailureDomain], node.Labels[v1.LabelZoneFailureDomainStable], ins.Zone)
		return false
	}
	regionEqual := node.Labels[v1.LabelZoneRegion] == ins.Region &&
		node.Labels[v1.LabelZoneRegionStable] == ins.Region
	if zoneEqual == false {
		klog.Errorf("node.Labels[v1.LabelZoneRegion]:%s,node.Labels[v1.LabelZoneRegionStable]:%s, ins.Region:%s",
			node.Labels[v1.LabelZoneRegion], node.Labels[v1.LabelZoneRegionStable], ins.Region)
		return false
	}
	for _, add1 := range ins.Addresses {
		found := false
		for _, add2 := range node.Status.Addresses {
			if add1.Type == add2.Type && add1.Address == add2.Address {
				found = true
				break
			}
		}
		if !found {
			klog.Errorf("node address:%#v", node.Status.Addresses)
			klog.Errorf("instance Addresses spec:%#v", ins.Addresses)
			return false
		}
	}

	return typeEqual && zoneEqual && regionEqual
}

func (f *Framework) DeleteRouteEntry(node *v1.Node) error {
	tables, err := f.Client.CloudClient.ListRouteTables(context.TODO(), options.TestConfig.VPCID)
	if err != nil {
		return err
	}

	for _, t := range tables {
		routes, err := f.Client.CloudClient.ListRoute(context.TODO(), t)
		if err != nil {
			return fmt.Errorf("list route error: %s", err.Error())
		}
		for _, route := range routes {
			if route.DestinationCIDR == node.Spec.PodCIDR && route.ProviderId == node.Spec.ProviderID {
				err := f.Client.CloudClient.DeleteRoute(context.TODO(), t, node.Spec.ProviderID, node.Spec.PodCIDR)
				if err != nil {
					return err
				}
				klog.Infof("successfully delete route for node %s,ins id: %s,cidr: %s",
					node.Name, node.Spec.ProviderID, node.Spec.PodCIDR)
			}
		}
	}
	return nil
}

func (f *Framework) ExpectRouteEqual() error {
	var (
		tables []string
		err    error
	)

	if ctrlCfg.CloudCFG.Global.RouteTableIDS != "" {
		tables = strings.Split(ctrlCfg.CloudCFG.Global.RouteTableIDS, ",")
	} else {
		tables, err = f.Client.CloudClient.ListRouteTables(context.TODO(), options.TestConfig.VPCID)
		if err != nil {
			return err
		}
	}

	var retErr error
	_ = wait.PollImmediate(30*time.Second, 5*time.Minute, func() (done bool, err error) {
		nodes, err := f.Client.KubeClient.ListNodes()
		if err != nil {
			retErr = err
			return false, nil
		}

		for _, node := range nodes {
			if isVKNode(node) {
				continue
			}
			found := false
			for _, t := range tables {
				routes, err := f.Client.CloudClient.ListRoute(context.TODO(), t)
				if err != nil {
					retErr = err
					return false, nil
				}
				if len(routes) == 0 {
					return false, nil
				}
				for _, route := range routes {
					if route.DestinationCIDR == node.Spec.PodCIDR && route.ProviderId == node.Spec.ProviderID {
						found = true
						break
					}
				}
			}
			if !found {
				tables, _ := json.Marshal(tables)
				retErr = fmt.Errorf("node %s do not have route in tables %s of vpc %s ",
					node.Name, string(tables), options.TestConfig.VPCID)
				return false, nil
			}
		}
		retErr = nil
		return true, nil
	})

	return retErr
}

func (f *Framework) FindLoadBalancer() (*v1.Service, *model.LoadBalancer, error) {
	// wait until service created successfully
	var svc *v1.Service
	err := wait.PollImmediate(5*time.Second, 30*time.Second, func() (done bool, err error) {
		svc, err = f.Client.KubeClient.GetService()
		if err != nil {
			return false, nil
		}
		if len(svc.Status.LoadBalancer.Ingress) == 1 &&
			(svc.Status.LoadBalancer.Ingress[0].IP != "" ||
				svc.Status.LoadBalancer.Ingress[0].Hostname != "") {
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		return svc, nil, err
	}

	remote, err := buildRemoteModel(f, svc)
	if err != nil {
		return svc, nil, fmt.Errorf("build lb remote model error: %s", err.Error())
	}
	if remote.LoadBalancerAttribute.LoadBalancerId == "" {
		return svc, nil, fmt.Errorf("slb is nil")
	}
	return svc, remote, nil
}

func buildRemoteModel(f *Framework, svc *v1.Service) (*model.LoadBalancer, error) {
	builder := &service.ModelBuilder{
		LoadBalancerMgr: service.NewLoadBalancerManager(f.Client.CloudClient),
		ListenerMgr:     service.NewListenerManager(f.Client.CloudClient),
		VGroupMgr:       service.NewVGroupManager(f.Client.RuntimeClient, f.Client.CloudClient),
	}
	reqCtx := &service.RequestContext{
		Service: svc,
		Anno:    service.NewAnnotationRequest(svc),
	}
	return builder.Instance(service.RemoteModel).Build(reqCtx)
}
