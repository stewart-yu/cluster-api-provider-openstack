package loadbalancer

import (
	"errors"
	"fmt"
	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/loadbalancer/v2/listeners"
	"github.com/gophercloud/gophercloud/openstack/loadbalancer/v2/loadbalancers"
	"github.com/gophercloud/gophercloud/openstack/loadbalancer/v2/monitors"
	"github.com/gophercloud/gophercloud/openstack/loadbalancer/v2/pools"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/layer3/floatingips"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog"
	infrav1 "sigs.k8s.io/cluster-api-provider-openstack/api/v1alpha2"
	"sigs.k8s.io/cluster-api/api/v1alpha2"
	"sigs.k8s.io/cluster-api/util"
	"time"
)

func (s *Service) ReconcileLoadBalancer(clusterName string, openStackCluster *infrav1.OpenStackCluster) error {

	if openStackCluster.Spec.ExternalNetworkID == "" {
		klog.V(3).Infof("No need to create loadbalancer, due to missing ExternalNetworkID")
		return nil
	}
	if openStackCluster.Spec.APIServerLoadBalancerFloatingIP == "" {
		klog.V(3).Infof("No need to create loadbalancer, due to missing APIServerLoadBalancerFloatingIP")
		return nil
	}
	if openStackCluster.Spec.APIServerLoadBalancerPort == 0 {
		klog.V(3).Infof("No need to create loadbalancer, due to missing APIServerLoadBalancerPort")
		return nil
	}

	loadBalancerName := fmt.Sprintf("%s-cluster-%s-%s", networkPrefix, clusterName, kubeapiLBSuffix)
	klog.Infof("Reconciling loadbalancer %s", loadBalancerName)

	// lb
	lb, err := checkIfLbExists(s.loadbalancerClient, loadBalancerName)
	if err != nil {
		return err
	}
	if lb == nil {
		klog.Infof("Creating loadbalancer %s", loadBalancerName)
		lbCreateOpts := loadbalancers.CreateOpts{
			Name:        loadBalancerName,
			VipSubnetID: openStackCluster.Status.Network.Subnet.ID,
		}

		lb, err = loadbalancers.Create(s.loadbalancerClient, lbCreateOpts).Extract()
		if err != nil {
			return fmt.Errorf("error creating loadbalancer: %s", err)
		}
		err = waitForLoadBalancer(s.loadbalancerClient, lb.ID, "ACTIVE")
		if err != nil {
			return err
		}
	}

	// floating ip
	fp, err := checkIfFloatingIPExists(s.networkingClient, openStackCluster.Spec.APIServerLoadBalancerFloatingIP)
	if err != nil {
		return err
	}
	if fp == nil {
		klog.Infof("Creating floating ip %s", openStackCluster.Spec.APIServerLoadBalancerFloatingIP)
		fpCreateOpts := &floatingips.CreateOpts{
			FloatingIP:        openStackCluster.Spec.APIServerLoadBalancerFloatingIP,
			FloatingNetworkID: openStackCluster.Spec.ExternalNetworkID,
		}
		fp, err = floatingips.Create(s.networkingClient, fpCreateOpts).Extract()
		if err != nil {
			return fmt.Errorf("error allocating floating IP: %s", err)
		}
	}

	// associate floating ip
	klog.Infof("Associating floating ip %s", openStackCluster.Spec.APIServerLoadBalancerFloatingIP)
	fpUpdateOpts := &floatingips.UpdateOpts{
		PortID: &lb.VipPortID,
	}
	fp, err = floatingips.Update(s.networkingClient, fp.ID, fpUpdateOpts).Extract()
	if err != nil {
		return fmt.Errorf("error allocating floating IP: %s", err)
	}
	err = waitForFloatingIP(s.networkingClient, fp.ID, "ACTIVE")
	if err != nil {
		return err
	}

	// lb listener
	portList := []int{openStackCluster.Spec.APIServerLoadBalancerPort}
	portList = append(portList, openStackCluster.Spec.APIServerLoadBalancerAdditionalPorts...)
	for _, port := range portList {
		lbPortObjectsName := fmt.Sprintf("%s-%d", loadBalancerName, port)

		listener, err := checkIfListenerExists(s.loadbalancerClient, lbPortObjectsName)
		if err != nil {
			return err
		}
		if listener == nil {
			klog.Infof("Creating lb listener %s", lbPortObjectsName)
			listenerCreateOpts := listeners.CreateOpts{
				Name:           lbPortObjectsName,
				Protocol:       "TCP",
				ProtocolPort:   port,
				LoadbalancerID: lb.ID,
			}
			listener, err = listeners.Create(s.loadbalancerClient, listenerCreateOpts).Extract()
			if err != nil {
				return fmt.Errorf("error creating listener: %s", err)
			}
			err = waitForLoadBalancer(s.loadbalancerClient, lb.ID, "ACTIVE")
			if err != nil {
				return err
			}
			err = waitForListener(s.loadbalancerClient, listener.ID, "ACTIVE")
			if err != nil {
				return err
			}
		}

		// lb pool
		pool, err := checkIfPoolExists(s.loadbalancerClient, lbPortObjectsName)
		if err != nil {
			return err
		}
		if pool == nil {
			klog.Infof("Creating lb pool %s", lbPortObjectsName)
			poolCreateOpts := pools.CreateOpts{
				Name:       lbPortObjectsName,
				Protocol:   "TCP",
				LBMethod:   pools.LBMethodRoundRobin,
				ListenerID: listener.ID,
			}
			pool, err = pools.Create(s.loadbalancerClient, poolCreateOpts).Extract()
			if err != nil {
				return fmt.Errorf("error creating pool: %s", err)
			}
			err = waitForLoadBalancer(s.loadbalancerClient, lb.ID, "ACTIVE")
			if err != nil {
				return err
			}
		}

		// lb monitor
		monitor, err := checkIfMonitorExists(s.loadbalancerClient, lbPortObjectsName)
		if err != nil {
			return err
		}
		if monitor == nil {
			klog.Infof("Creating lb monitor %s", lbPortObjectsName)
			monitorCreateOpts := monitors.CreateOpts{
				Name:       lbPortObjectsName,
				PoolID:     pool.ID,
				Type:       "TCP",
				Delay:      30,
				Timeout:    5,
				MaxRetries: 3,
			}
			_, err = monitors.Create(s.loadbalancerClient, monitorCreateOpts).Extract()
			if err != nil {
				return fmt.Errorf("error creating monitor: %s", err)
			}
			err = waitForLoadBalancer(s.loadbalancerClient, lb.ID, "ACTIVE")
			if err != nil {
				return err
			}
		}
	}

	openStackCluster.Status.Network.APIServerLoadBalancer = &infrav1.LoadBalancer{
		Name:       lb.Name,
		ID:         lb.ID,
		InternalIP: lb.VipAddress,
		IP:         fp.FloatingIP,
	}
	return nil
}

func (s *Service) ReconcileLoadBalancerMember(clusterName string, machine *v1alpha2.Machine, openStackMachine *infrav1.OpenStackMachine, openStackCluster *infrav1.OpenStackCluster, ip string) error {
	if !util.IsControlPlaneMachine(machine) {
		return nil
	}

	if openStackCluster.Status.Network == nil {
		return errors.New("network is not yet available in openStackCluster.Status")
	}
	if openStackCluster.Status.Network.Subnet == nil {
		return errors.New("network.Subnet is not yet available in openStackCluster.Status")
	}
	if openStackCluster.Status.Network.APIServerLoadBalancer == nil {
		return errors.New("network.APIServerLoadBalancer is not yet available in openStackCluster.Status")
	}

	loadBalancerName := fmt.Sprintf("%s-cluster-%s-%s", networkPrefix, clusterName, kubeapiLBSuffix)
	klog.Infof("Reconciling loadbalancer %s for member %s", loadBalancerName, openStackMachine.Name)

	lbID := openStackCluster.Status.Network.APIServerLoadBalancer.ID
	subnetID := openStackCluster.Status.Network.Subnet.ID

	portList := []int{openStackCluster.Spec.APIServerLoadBalancerPort}
	portList = append(portList, openStackCluster.Spec.APIServerLoadBalancerAdditionalPorts...)
	for _, port := range portList {
		lbPortObjectsName := fmt.Sprintf("%s-%d", loadBalancerName, port)
		name := lbPortObjectsName + "-" + openStackMachine.Name

		pool, err := checkIfPoolExists(s.loadbalancerClient, lbPortObjectsName)
		if err != nil {
			return err
		}
		if pool == nil {
			return errors.New("loadbalancer pool does not exist yet")
		}

		lbMember, err := checkIfLbMemberExists(s.loadbalancerClient, pool.ID, name)
		if err != nil {
			return err
		}

		if lbMember != nil {
			// check if we have to recreate the LB Member
			if lbMember.Address == ip {
				// nothing to do return
				return nil
			}

			klog.Infof("Deleting lb member %s (because the IP of the machine changed)", name)

			// lb member changed so let's delete it so we can create it again with the correct IP
			err = waitForLoadBalancer(s.loadbalancerClient, lbID, "ACTIVE")
			if err != nil {
				return err
			}
			err = pools.DeleteMember(s.loadbalancerClient, pool.ID, lbMember.ID).ExtractErr()
			if err != nil {
				return fmt.Errorf("error deleting lbmember: %s", err)
			}
			err = waitForLoadBalancer(s.loadbalancerClient, lbID, "ACTIVE")
			if err != nil {
				return err
			}
		}

		klog.Infof("Creating lb member %s", name)

		// if we got to this point we should either create or re-create the lb member
		lbMemberOpts := pools.CreateMemberOpts{
			Name:         name,
			ProtocolPort: port,
			Address:      ip,
			SubnetID:     subnetID,
		}

		err = waitForLoadBalancer(s.loadbalancerClient, lbID, "ACTIVE")
		if err != nil {
			return err
		}
		lbMember, err = pools.CreateMember(s.loadbalancerClient, pool.ID, lbMemberOpts).Extract()
		if err != nil {
			return fmt.Errorf("error create lbmember: %s", err)
		}
		err = waitForLoadBalancer(s.loadbalancerClient, lbID, "ACTIVE")
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) DeleteLoadBalancer(clusterName string, openStackCluster *infrav1.OpenStackCluster) error {
	loadBalancerName := fmt.Sprintf("%s-cluster-%s-%s", networkPrefix, clusterName, kubeapiLBSuffix)
	lb, err := checkIfLbExists(s.loadbalancerClient, loadBalancerName)
	if err != nil {
		return err
	}

	// only Octavia supports Cascade
	if openStackCluster.Spec.UseOctavia {
		deleteOpts := loadbalancers.DeleteOpts{
			Cascade: true,
		}
		klog.Infof("Deleting loadbalancer %s", loadBalancerName)
		err = loadbalancers.Delete(s.loadbalancerClient, lb.ID, deleteOpts).ExtractErr()
		if err != nil {
			return fmt.Errorf("error deleting loadbalancer: %s", err)
		}
	} else {
		if err := s.deleteLoadBalancerNeutronV2(lb.ID); err != nil {
			return fmt.Errorf("error deleting loadbalancer: %s", err)
		}
	}

	// floating ip
	// TODO: need delete floating IP if it's created when doing the cluster provisioning
	// but keep the floating ips if it's original exist (probably should store it in the
	// Cluster status if the floating ip has been created by us)
	return nil
}

// ref: https://github.com/kubernetes/kubernetes/blob/7f23a743e8c23ac6489340bbb34fa6f1d392db9d/pkg/cloudprovider/providers/openstack/openstack_loadbalancer.go#L1452
func (s *Service) deleteLoadBalancerNeutronV2(id string) error {

	lb, err := loadbalancers.Get(s.loadbalancerClient, id).Extract()
	if err != nil {
		return fmt.Errorf("unable to get loadbalancer: %v", err)
	}

	// get all listeners
	r, err := listeners.List(s.loadbalancerClient, listeners.ListOpts{LoadbalancerID: lb.ID}).AllPages()
	if err != nil {
		return fmt.Errorf("unable to list listeners of loadbalancer %s: %v", lb.ID, err)
	}
	lbListeners, err := listeners.ExtractListeners(r)
	if err != nil {
		return fmt.Errorf("unable to extract listeners: %v", err)
	}

	// get all pools and healthmonitors for this lb
	r, err = pools.List(s.loadbalancerClient, pools.ListOpts{LoadbalancerID: lb.ID}).AllPages()
	if err != nil {
		return fmt.Errorf("unable to list pools for laodbalancer %s: %v", lb.ID, err)
	}
	lbPools, err := pools.ExtractPools(r)
	if err != nil {
		return fmt.Errorf("unable to extract pools for laodbalancer %s: %v", lb.ID, err)
	}

	for _, pool := range lbPools {
		// delete the monitors
		if pool.MonitorID != "" {
			klog.Infof("Deleting lb monitor %s", pool.MonitorID)
			err := monitors.Delete(s.loadbalancerClient, pool.MonitorID).ExtractErr()
			if err != nil {
				return fmt.Errorf("error deleting lbaas monitor %s: %v", pool.MonitorID, err)
			}
			if err = waitForLoadBalancer(s.loadbalancerClient, lb.ID, "ACTIVE"); err != nil {
				return fmt.Errorf("loadbalancer %s did not get back to %s state in time", lb.ID, "Active")
			}
		}

		// get all members of pool
		r, err := pools.ListMembers(s.loadbalancerClient, pool.ID, pools.ListMembersOpts{}).AllPages()
		if err != nil {
			return fmt.Errorf("error listing loadbalancer members of pool %s: %v", pool.ID, err)
		}
		members, err := pools.ExtractMembers(r)
		if err != nil {
			return fmt.Errorf("unable to extract members: %v", err)
		}
		// delete all members of pool
		for _, member := range members {
			klog.Infof("Deleting lb member %s (%s)", member.Name, member.ID)
			err := pools.DeleteMember(s.loadbalancerClient, pool.ID, member.ID).ExtractErr()
			if err != nil {
				return fmt.Errorf("error deleting lbaas member %s on pool %s: %v", member.ID, pool.ID, err)
			}
			if err = waitForLoadBalancer(s.loadbalancerClient, lb.ID, "ACTIVE"); err != nil {
				return fmt.Errorf("loadbalancer %s did not get back to %s state in time", lb.ID, "ACTIVE")
			}
		}

		// delete pool
		klog.Infof("Deleting lb pool %s (%s)", pool.Name, pool.ID)
		err = pools.Delete(s.loadbalancerClient, pool.ID).ExtractErr()
		if err != nil {
			return fmt.Errorf("error deleting lbaas pool %s: %v", pool.ID, err)
		}
		if err = waitForLoadBalancer(s.loadbalancerClient, lb.ID, "ACTIVE"); err != nil {
			return fmt.Errorf("loadbalancer %s did not get back to %s state in time", lb.ID, "ACTIVE")
		}
	}

	// delete all listeners
	for _, listener := range lbListeners {
		klog.Infof("Deleting lb listener %s (%s)", listener.Name, listener.ID)
		err = listeners.Delete(s.loadbalancerClient, listener.ID).ExtractErr()
		if err != nil {
			return fmt.Errorf("error deleting lbaas listener %s: %v", listener.ID, err)
		}
		if err = waitForLoadBalancer(s.loadbalancerClient, lb.ID, "ACTIVE"); err != nil {
			return fmt.Errorf("loadbalancer %s did not get back to %s state in time", lb.ID, "ACTIVE")
		}
	}

	// delete loadbalancer
	klog.Infof("Deleting loadbalancer %s (%s)", lb.Name, lb.ID)
	if err = loadbalancers.Delete(s.loadbalancerClient, lb.ID, loadbalancers.DeleteOpts{}).ExtractErr(); err != nil {
		return fmt.Errorf("error deleting lbaas %s: %v", lb.ID, err)
	}

	return nil
}

func (s *Service) DeleteLoadBalancerMember(clusterName string, machine *v1alpha2.Machine, openStackMachine *infrav1.OpenStackMachine, openStackCluster *infrav1.OpenStackCluster) error {

	if openStackMachine == nil || !util.IsControlPlaneMachine(machine) {
		return nil
	}

	loadBalancerName := fmt.Sprintf("%s-cluster-%s-%s", networkPrefix, clusterName, kubeapiLBSuffix)
	klog.Infof("Reconciling loadbalancer %s", loadBalancerName)

	lbID := openStackCluster.Status.Network.APIServerLoadBalancer.ID

	portList := []int{openStackCluster.Spec.APIServerLoadBalancerPort}
	portList = append(portList, openStackCluster.Spec.APIServerLoadBalancerAdditionalPorts...)
	for _, port := range portList {
		lbPortObjectsName := fmt.Sprintf("%s-%d", loadBalancerName, port)
		name := lbPortObjectsName + "-" + openStackMachine.Name

		pool, err := checkIfPoolExists(s.loadbalancerClient, lbPortObjectsName)
		if err != nil {
			return err
		}
		if pool == nil {
			klog.Infof("Pool %s does not exist", lbPortObjectsName)
			continue
		}

		lbMember, err := checkIfLbMemberExists(s.loadbalancerClient, pool.ID, name)
		if err != nil {
			return err
		}

		if lbMember != nil {

			// lb member changed so let's delete it so we can create it again with the correct IP
			err = waitForLoadBalancer(s.loadbalancerClient, lbID, "ACTIVE")
			if err != nil {
				return err
			}
			err = pools.DeleteMember(s.loadbalancerClient, pool.ID, lbMember.ID).ExtractErr()
			if err != nil {
				return fmt.Errorf("error deleting lbmember: %s", err)
			}
			err = waitForLoadBalancer(s.loadbalancerClient, lbID, "ACTIVE")
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func checkIfLbExists(client *gophercloud.ServiceClient, name string) (*loadbalancers.LoadBalancer, error) {
	allPages, err := loadbalancers.List(client, loadbalancers.ListOpts{Name: name}).AllPages()
	if err != nil {
		return nil, err
	}
	lbList, err := loadbalancers.ExtractLoadBalancers(allPages)
	if err != nil {
		return nil, err
	}
	if len(lbList) == 0 {
		return nil, nil
	}
	return &lbList[0], nil
}

func checkIfFloatingIPExists(client *gophercloud.ServiceClient, ip string) (*floatingips.FloatingIP, error) {
	allPages, err := floatingips.List(client, floatingips.ListOpts{FloatingIP: ip}).AllPages()
	if err != nil {
		return nil, err
	}
	fpList, err := floatingips.ExtractFloatingIPs(allPages)
	if err != nil {
		return nil, err
	}
	if len(fpList) == 0 {
		return nil, nil
	}
	return &fpList[0], nil
}

func checkIfListenerExists(client *gophercloud.ServiceClient, name string) (*listeners.Listener, error) {
	allPages, err := listeners.List(client, listeners.ListOpts{Name: name}).AllPages()
	if err != nil {
		return nil, err
	}
	listenerList, err := listeners.ExtractListeners(allPages)
	if err != nil {
		return nil, err
	}
	if len(listenerList) == 0 {
		return nil, nil
	}
	return &listenerList[0], nil
}

func checkIfPoolExists(client *gophercloud.ServiceClient, name string) (*pools.Pool, error) {
	allPages, err := pools.List(client, pools.ListOpts{Name: name}).AllPages()
	if err != nil {
		return nil, err
	}
	poolList, err := pools.ExtractPools(allPages)
	if err != nil {
		return nil, err
	}
	if len(poolList) == 0 {
		return nil, nil
	}
	return &poolList[0], nil
}

func checkIfMonitorExists(client *gophercloud.ServiceClient, name string) (*monitors.Monitor, error) {
	allPages, err := monitors.List(client, monitors.ListOpts{Name: name}).AllPages()
	if err != nil {
		return nil, err
	}
	monitorList, err := monitors.ExtractMonitors(allPages)
	if err != nil {
		return nil, err
	}
	if len(monitorList) == 0 {
		return nil, nil
	}
	return &monitorList[0], nil
}

func checkIfLbMemberExists(client *gophercloud.ServiceClient, poolID, name string) (*pools.Member, error) {
	allPages, err := pools.ListMembers(client, poolID, pools.ListMembersOpts{Name: name}).AllPages()
	if err != nil {
		return nil, err
	}
	lbMemberList, err := pools.ExtractMembers(allPages)
	if err != nil {
		return nil, err
	}
	if len(lbMemberList) == 0 {
		return nil, nil
	}
	return &lbMemberList[0], nil
}

var backoff = wait.Backoff{
	Steps:    10,
	Duration: 30 * time.Second,
	Factor:   1.0,
	Jitter:   0.1,
}

// Possible LoadBalancer states are documented here: https://developer.openstack.org/api-ref/network/v2/?expanded=show-load-balancer-status-tree-detail#load-balancer-statuses
func waitForLoadBalancer(client *gophercloud.ServiceClient, id, target string) error {
	klog.Infof("Waiting for loadbalancer %s to become %s.", id, target)
	return wait.ExponentialBackoff(backoff, func() (bool, error) {
		lb, err := loadbalancers.Get(client, id).Extract()
		if err != nil {
			return false, err
		}
		return lb.ProvisioningStatus == target, nil
	})
}

func waitForFloatingIP(client *gophercloud.ServiceClient, id, target string) error {
	klog.Infof("Waiting for floatingip %s to become %s.", id, target)
	return wait.ExponentialBackoff(backoff, func() (bool, error) {
		fp, err := floatingips.Get(client, id).Extract()
		if err != nil {
			return false, err
		}
		return fp.Status == target, nil
	})
}

func waitForListener(client *gophercloud.ServiceClient, id, target string) error {
	klog.Infof("Waiting for listener %s to become %s.", id, target)
	return wait.ExponentialBackoff(backoff, func() (bool, error) {
		_, err := listeners.Get(client, id).Extract()
		if err != nil {
			return false, err
		}
		// The listener resource has no Status attribute, so a successful Get is the best we can do
		return true, nil
	})
}
