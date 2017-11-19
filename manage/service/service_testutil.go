package manageservice

import (
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/golang/glog"
	"golang.org/x/net/context"

	"github.com/cloudstax/firecamp/common"
	"github.com/cloudstax/firecamp/db"
	"github.com/cloudstax/firecamp/dns"
	"github.com/cloudstax/firecamp/manage"
	"github.com/cloudstax/firecamp/server"
	"github.com/cloudstax/firecamp/utils"
)

func TestUtil_ServiceCreation(t *testing.T, s *ManageService, dbIns db.DB, serverIns *server.LoopServer, requireStaticIP bool) {
	cluster := "cluster1"
	servicePrefix := "service-"
	service := "service-0"

	volSize := int64(1)
	servicevol := &common.ServiceVolume{
		VolumeType:   common.VolumeTypeGPSSD,
		VolumeSizeGB: volSize,
	}

	az := "az-west"

	registerDNS := true
	domain := "example.com"
	vpcID := "vpc-1"
	region := "us-west-1"

	max := 10
	idx := 0

	firstIP := 4

	ctx := context.Background()

	// service map, key: service name, value: ""
	svcMap := make(map[string]string)

	taskCount1 := 3
	for idx < 3 {
		service = servicePrefix + strconv.Itoa(idx)

		// create the config files for replicas
		replicaCfgs := make([]*manage.ReplicaConfig, taskCount1)
		for i := 0; i < taskCount1; i++ {
			cfg := &manage.ReplicaConfigFile{FileName: service, Content: service}
			configs := []*manage.ReplicaConfigFile{cfg}
			replicaCfg := &manage.ReplicaConfig{Zone: az, Configs: configs}
			replicaCfgs[i] = replicaCfg
		}

		var journalVol *common.ServiceVolume
		if idx == 2 {
			journalVol = &common.ServiceVolume{
				VolumeType:   common.VolumeTypeGPSSD,
				VolumeSizeGB: 1,
				Iops:         100,
			}
		}

		req := &manage.CreateServiceRequest{
			Service: &manage.ServiceCommonRequest{
				Region:      region,
				Cluster:     cluster,
				ServiceName: service,
			},
			Replicas:        int64(taskCount1),
			Volume:          servicevol,
			JournalVolume:   journalVol,
			RegisterDNS:     registerDNS,
			RequireStaticIP: requireStaticIP,
			ReplicaConfigs:  replicaCfgs,
		}

		svcUUID, err := s.CreateService(ctx, req, domain, vpcID)
		if err != nil {
			t.Fatalf("CreateService error %s, cluster %s, service %s taskCount1 %d",
				err, cluster, service, taskCount1)
		}

		glog.Infoln("created service", service, "uuid", svcUUID)

		err = s.SetServiceInitialized(ctx, cluster, service)
		if err != nil {
			t.Fatalf("SetServiceInitialized error %s, cluster %s, service %s, taskCount1 %d",
				err, cluster, service, taskCount1)
		}

		// get service attr to verify
		sattr, err := dbIns.GetServiceAttr(ctx, svcUUID)
		if err != nil {
			t.Fatalf("GetServiceAttr error %s, cluster %s, service %s", err, cluster, service)
		}
		if idx == 2 {
			if len(sattr.Volumes.PrimaryDeviceName) == 0 || len(sattr.Volumes.JournalDeviceName) == 0 {
				t.Fatalf("expect journal device for service %s, service volumes %s", service, sattr.Volumes)
			}
		} else {
			if len(sattr.Volumes.PrimaryDeviceName) == 0 || len(sattr.Volumes.JournalDeviceName) != 0 {
				t.Fatalf("not expect journal device for service %s, service volumes %s", service, sattr.Volumes)
			}
		}

		// list service members to verify
		memberItems, err := dbIns.ListServiceMembers(ctx, svcUUID)
		if err != nil || len(memberItems) != taskCount1 {
			t.Fatalf("got %d, expect %d serviceMembers for service %s", len(memberItems), taskCount1, service)
		}
		sort.Slice(memberItems, func(i, j int) bool {
			idx1, err := utils.GetServiceMemberIndex(memberItems[i].MemberName)
			if err != nil {
				t.Fatalf("GetServiceMemberIndex error %s, %s", err, memberItems[i])
			}
			idx2, err := utils.GetServiceMemberIndex(memberItems[j].MemberName)
			if err != nil {
				t.Fatalf("GetServiceMemberIndex error %s, %s", err, memberItems[j])
			}
			return idx1 < idx2
		})
		for i, member := range memberItems {
			name := utils.GenServiceMemberName(service, int64(i))
			if member.MemberName != name {
				t.Fatalf("expect member name %s, get %s", name, member)
			}
			if requireStaticIP {
				ipPrefix, _, _, _ := serverIns.GetCidrBlock()
				ip := ipPrefix + strconv.Itoa(firstIP+i)
				if member.StaticIP != ip {
					t.Fatalf("expect ip %s for member %s", ip, member)
				}
			}
			if idx == 2 {
				if len(member.Volumes.PrimaryVolumeID) == 0 || len(member.Volumes.PrimaryDeviceName) == 0 ||
					len(member.Volumes.JournalVolumeID) == 0 || len(member.Volumes.JournalDeviceName) == 0 {
					t.Fatalf("expect journal device for service %s, service volumes %s", service, member.Volumes)
				}
			} else {
				if len(member.Volumes.PrimaryVolumeID) == 0 || len(member.Volumes.PrimaryDeviceName) == 0 ||
					len(member.Volumes.JournalVolumeID) != 0 || len(member.Volumes.JournalDeviceName) != 0 {
					t.Fatalf("not expect journal device for service %s, service volumes %s", service, member.Volumes)
				}
			}
		}
		// increase the firstIP for the next service
		firstIP = firstIP + taskCount1

		svcMap[service] = ""
		idx++
	}
	// verify the Device, Service, ServiceAttr and ServiceMember
	deviceItems, err := dbIns.ListDevices(ctx, cluster)
	if err != nil || len(deviceItems) != 4 {
		t.Fatalf("ListDevices error %s, deviceItems %s", err, deviceItems)
	}
	serviceItems, err := dbIns.ListServices(ctx, cluster)
	if err != nil || len(serviceItems) != 3 {
		t.Fatalf("ListServices error %s, serviceItems %s", err, serviceItems)
	}
	for _, svc := range serviceItems {
		_, ok := svcMap[svc.ServiceName]
		if !ok {
			t.Fatalf("unexpected service %s, items %d", svc, serviceItems)
		}
		attr, err := dbIns.GetServiceAttr(ctx, svc.ServiceUUID)
		if err != nil || attr.ServiceName != svc.ServiceName {
			t.Fatalf("get service attr error %s service %s attr %s, expect ", err, svc, attr)
		}
		memberItems, err := dbIns.ListServiceMembers(ctx, svc.ServiceUUID)
		if err != nil || len(memberItems) != taskCount1 {
			t.Fatalf("got %d, expect %d serviceMembers for service %s attr %s", len(memberItems), taskCount1, svc, attr)
		}
	}

	// create 2 more services
	taskCount2 := 2
	for idx < 5 {
		service = servicePrefix + strconv.Itoa(idx)

		// create the config files for replicas
		replicaCfgs := make([]*manage.ReplicaConfig, taskCount2)
		for i := 0; i < taskCount2; i++ {
			cfg := &manage.ReplicaConfigFile{FileName: service, Content: service}
			configs := []*manage.ReplicaConfigFile{cfg}
			replicaCfg := &manage.ReplicaConfig{Zone: az, Configs: configs}
			replicaCfgs[i] = replicaCfg
		}

		var journalVol *common.ServiceVolume
		if idx == 4 {
			journalVol = &common.ServiceVolume{
				VolumeType:   common.VolumeTypeGPSSD,
				VolumeSizeGB: 1,
				Iops:         100,
			}
		}

		req := &manage.CreateServiceRequest{
			Service: &manage.ServiceCommonRequest{
				Region:      region,
				Cluster:     cluster,
				ServiceName: service,
			},
			Replicas:        int64(taskCount2),
			Volume:          servicevol,
			JournalVolume:   journalVol,
			RegisterDNS:     registerDNS,
			RequireStaticIP: requireStaticIP,
			ReplicaConfigs:  replicaCfgs,
		}

		svcUUID, err := s.CreateService(ctx, req, domain, vpcID)
		if err != nil {
			t.Fatalf("CreateService error %s, cluster %s, service %s taskCount2 %d",
				err, cluster, service, taskCount2)
		}

		glog.Infoln("created service", service, "uuid", svcUUID)

		err = s.SetServiceInitialized(ctx, cluster, service)
		if err != nil {
			t.Fatalf("SetServiceInitialized error %s, cluster %s, service %s, taskCount2 %d",
				err, cluster, service, taskCount2)
		}

		// get service attr to verify
		sattr, err := dbIns.GetServiceAttr(ctx, svcUUID)
		if err != nil {
			t.Fatalf("GetServiceAttr error %s, cluster %s, service %s", err, cluster, service)
		}
		if idx == 4 {
			if len(sattr.Volumes.PrimaryDeviceName) == 0 || len(sattr.Volumes.JournalDeviceName) == 0 {
				t.Fatalf("expect journal device for service %s, service volumes %s", service, sattr.Volumes)
			}
		} else {
			if len(sattr.Volumes.PrimaryDeviceName) == 0 || len(sattr.Volumes.JournalDeviceName) != 0 {
				t.Fatalf("not expect journal device for service %s, service volumes %s", service, sattr.Volumes)
			}
		}

		// list service members to verify
		memberItems, err := dbIns.ListServiceMembers(ctx, svcUUID)
		if err != nil || len(memberItems) != taskCount2 {
			t.Fatalf("got %d, expect %d serviceMembers for service %s", len(memberItems), taskCount2, service)
		}
		sort.Slice(memberItems, func(i, j int) bool {
			idx1, err := utils.GetServiceMemberIndex(memberItems[i].MemberName)
			if err != nil {
				t.Fatalf("GetServiceMemberIndex error %s, %s", err, memberItems[i])
			}
			idx2, err := utils.GetServiceMemberIndex(memberItems[j].MemberName)
			if err != nil {
				t.Fatalf("GetServiceMemberIndex error %s, %s", err, memberItems[j])
			}
			return idx1 < idx2
		})
		for i, member := range memberItems {
			name := utils.GenServiceMemberName(service, int64(i))
			if member.MemberName != name {
				t.Fatalf("expect member name %s, get %s", name, member)
			}
			if requireStaticIP {
				ipPrefix, _, _, _ := serverIns.GetCidrBlock()
				ip := ipPrefix + strconv.Itoa(firstIP+i)
				if member.StaticIP != ip {
					t.Fatalf("expect ip %s for member %s", ip, member)
				}
			}
			if idx == 4 {
				if len(member.Volumes.PrimaryVolumeID) == 0 || len(member.Volumes.PrimaryDeviceName) == 0 ||
					len(member.Volumes.JournalVolumeID) == 0 || len(member.Volumes.JournalDeviceName) == 0 {
					t.Fatalf("expect journal device for service %s, service volumes %s", service, member.Volumes)
				}
			} else {
				if len(member.Volumes.PrimaryVolumeID) == 0 || len(member.Volumes.PrimaryDeviceName) == 0 ||
					len(member.Volumes.JournalVolumeID) != 0 || len(member.Volumes.JournalDeviceName) != 0 {
					t.Fatalf("not expect journal device for service %s, service volumes %s", service, member.Volumes)
				}
			}
		}
		// increase the firstIP for the next service
		firstIP = firstIP + taskCount2

		svcMap[service] = ""
		idx++
	}
	// verify the Device, Service and ServiceMember
	deviceItems, err = dbIns.ListDevices(ctx, cluster)
	if err != nil || len(deviceItems) != 7 {
		t.Fatalf("ListDevices error %s, deviceItems %s", err, deviceItems)
	}
	serviceItems, err = dbIns.ListServices(ctx, cluster)
	if err != nil || len(serviceItems) != 5 {
		t.Fatalf("ListServices error %s, serviceItems %s", err, serviceItems)
	}
	for _, svc := range serviceItems {
		_, ok := svcMap[svc.ServiceName]
		if !ok {
			t.Fatalf("unexpected service %s, items %d", svc, serviceItems)
		}
		attr, err := dbIns.GetServiceAttr(ctx, svc.ServiceUUID)
		if err != nil || attr.ServiceName != svc.ServiceName {
			t.Fatalf("get service attr error %s service %s attr %s, expect ", err, svc, attr)
		}
		memberItems, err := dbIns.ListServiceMembers(ctx, svc.ServiceUUID)
		if err != nil {
			t.Fatalf("ListServiceMembers error %s, serviceUUID %s", err, svc.ServiceUUID)
		}
		seqstr := strings.TrimPrefix(svc.ServiceName, servicePrefix)
		seq, err := strconv.Atoi(seqstr)
		if err != nil {
			t.Fatalf("Atoi seqstr %s error %s for service %s", seqstr, err, svc)
		}
		if seq < 3 {
			if len(memberItems) != taskCount1 {
				t.Fatalf("got %d, expect %d serviceMembers for service %s", len(memberItems), taskCount1, svc)
			}
		} else {
			if seq < 5 {
				if len(memberItems) != taskCount2 {
					t.Fatalf("got %d, expect %d serviceMembers for service %s", len(memberItems), taskCount2, svc)
				}
			} else {
				t.Fatalf("unexpect seq %d, seqstr %s, service %s", seq, seqstr, svc)
			}
		}
	}

	// create 5 more services
	taskCount3 := 4
	for idx < max {
		service = servicePrefix + strconv.Itoa(idx)

		// create the config files for replicas
		replicaCfgs := make([]*manage.ReplicaConfig, taskCount3)
		for i := 0; i < taskCount3; i++ {
			cfg := &manage.ReplicaConfigFile{FileName: service, Content: service}
			configs := []*manage.ReplicaConfigFile{cfg}
			replicaCfg := &manage.ReplicaConfig{Zone: az, Configs: configs}
			replicaCfgs[i] = replicaCfg
		}

		req := &manage.CreateServiceRequest{
			Service: &manage.ServiceCommonRequest{
				Region:      region,
				Cluster:     cluster,
				ServiceName: service,
			},
			Replicas:        int64(taskCount3),
			Volume:          servicevol,
			RegisterDNS:     registerDNS,
			RequireStaticIP: requireStaticIP,
			ReplicaConfigs:  replicaCfgs,
		}

		svcUUID, err := s.CreateService(ctx, req, domain, vpcID)
		if err != nil {
			t.Fatalf("CreateService error %s, cluster %s, service %s taskCount3 %d",
				err, cluster, service, taskCount3)
		}

		err = s.SetServiceInitialized(ctx, cluster, service)
		if err != nil {
			t.Fatalf("SetServiceInitialized error %s, cluster %s, service %s, taskCount3 %d",
				err, cluster, service, taskCount3)
		}

		// list service members to verify
		memberItems, err := dbIns.ListServiceMembers(ctx, svcUUID)
		if err != nil || len(memberItems) != taskCount3 {
			t.Fatalf("got %d, expect %d serviceMembers for service %s", len(memberItems), taskCount3, service)
		}
		sort.Slice(memberItems, func(i, j int) bool {
			idx1, err := utils.GetServiceMemberIndex(memberItems[i].MemberName)
			if err != nil {
				t.Fatalf("GetServiceMemberIndex error %s, %s", err, memberItems[i])
			}
			idx2, err := utils.GetServiceMemberIndex(memberItems[j].MemberName)
			if err != nil {
				t.Fatalf("GetServiceMemberIndex error %s, %s", err, memberItems[j])
			}
			return idx1 < idx2
		})
		for i, member := range memberItems {
			name := utils.GenServiceMemberName(service, int64(i))
			if member.MemberName != name {
				t.Fatalf("expect member name %s, get %s", name, member)
			}
			if requireStaticIP {
				ipPrefix, _, _, _ := serverIns.GetCidrBlock()
				ip := ipPrefix + strconv.Itoa(firstIP+i)
				if member.StaticIP != ip {
					t.Fatalf("expect ip %s for member %s", ip, member)
				}
			}
		}
		// increase the firstIP for the next service
		firstIP = firstIP + taskCount3

		idx++
	}

	// list volumes of the service
	service = servicePrefix + strconv.Itoa(idx-1)
	vols, err := s.ListServiceVolumes(ctx, cluster, service)
	if err != nil || len(vols) != taskCount3 {
		t.Fatalf("ListServiceVolumes %s error % vols %s", service, err, vols)
	}

	// test service deletion
	vols, err = s.DeleteService(ctx, cluster, service)
	if err != nil {
		t.Fatalf("DeleteService %s error % vols %s", service, err, vols)
	}
}

func TestUtil_ServiceCreationRetry(t *testing.T, s *ManageService, dbIns db.DB, dnsIns dns.DNS, serverIns *server.LoopServer, requireStaticIP bool, requireJournalVolume bool) {
	cluster := "cluster1"
	az := "az-west"
	servicePrefix := "service-"
	taskCount := 3

	volSize := int64(1)
	servicevol := &common.ServiceVolume{
		VolumeType:   common.VolumeTypeGPSSD,
		VolumeSizeGB: volSize,
	}

	idx := 0
	firstIP := 4
	ipPrefix, _, _, _ := serverIns.GetCidrBlock()

	registerDNS := true
	domain := "example.com"
	vpcID := "vpc-1"
	region := "us-west-1"

	ctx := context.Background()

	service := servicePrefix + strconv.Itoa(idx)

	// create the config files for replicas
	replicaCfgs := make([]*manage.ReplicaConfig, taskCount)
	for i := 0; i < taskCount; i++ {
		cfg := &manage.ReplicaConfigFile{FileName: service, Content: service}
		configs := []*manage.ReplicaConfigFile{cfg}
		replicaCfg := &manage.ReplicaConfig{Zone: az, Configs: configs}
		replicaCfgs[i] = replicaCfg
	}

	req := &manage.CreateServiceRequest{
		Service: &manage.ServiceCommonRequest{
			Region:      region,
			Cluster:     cluster,
			ServiceName: service,
		},
		Replicas:        int64(taskCount),
		Volume:          servicevol,
		RegisterDNS:     registerDNS,
		RequireStaticIP: requireStaticIP,
		ReplicaConfigs:  replicaCfgs,
	}
	if requireJournalVolume {
		req.JournalVolume = &common.ServiceVolume{
			VolumeType:   common.VolumeTypeGPSSD,
			VolumeSizeGB: volSize,
			Iops:         1,
		}
	}

	// TODO test more cases. for example, service at different status, config file exists.
	// 1. device item exist
	dev, err := s.createDevice(ctx, cluster, service, "", "requuid")
	if err != nil || dev != "/dev/loop1" {
		t.Fatalf("createDevice error %s, expectDev /dev/loop1, got %s", err, dev)
	}
	svcUUID, err := s.CreateService(ctx, req, domain, vpcID)
	if err != nil {
		t.Fatalf("CreateService error %s, cluster %s, service %s, taskCount %d",
			err, cluster, service, taskCount)
	}
	// get service attr to verify
	sattr, err := dbIns.GetServiceAttr(ctx, svcUUID)
	if err != nil {
		t.Fatalf("GetServiceAttr error %s, cluster %s, service %s", err, cluster, service)
	}
	if sattr.Volumes.PrimaryDeviceName != "/dev/loop1" {
		t.Fatalf("expect primary device for service %s, service volumes %s", service, sattr.Volumes)
	}
	if requireJournalVolume {
		if sattr.Volumes.JournalDeviceName != "/dev/loop2" {
			t.Fatalf("expect journal device for service %s, service volumes %s", service, sattr.Volumes)
		}
	} else {
		if len(sattr.Volumes.JournalDeviceName) != 0 {
			t.Fatalf("not expect journal device for service %s, service volumes %s", service, sattr.Volumes)
		}
	}
	// list service members to verify
	memberItems, err := dbIns.ListServiceMembers(ctx, svcUUID)
	if err != nil || len(memberItems) != taskCount {
		t.Fatalf("got %d, expect %d serviceMembers for service %s", len(memberItems), taskCount, service)
	}
	sort.Slice(memberItems, func(i, j int) bool {
		idx1, err := utils.GetServiceMemberIndex(memberItems[i].MemberName)
		if err != nil {
			t.Fatalf("GetServiceMemberIndex error %s, %s", err, memberItems[i])
		}
		idx2, err := utils.GetServiceMemberIndex(memberItems[j].MemberName)
		if err != nil {
			t.Fatalf("GetServiceMemberIndex error %s, %s", err, memberItems[j])
		}
		return idx1 < idx2
	})
	for i, member := range memberItems {
		name := utils.GenServiceMemberName(service, int64(i))
		if member.MemberName != name {
			t.Fatalf("expect member name %s, get %s", name, member)
		}
		if requireStaticIP {
			ip := ipPrefix + strconv.Itoa(firstIP+i)
			if member.StaticIP != ip {
				t.Fatalf("expect ip %s for member %s", ip, member)
			}
		}
		if requireJournalVolume {
			if len(member.Volumes.PrimaryVolumeID) == 0 || len(member.Volumes.PrimaryDeviceName) == 0 ||
				len(member.Volumes.JournalVolumeID) == 0 || len(member.Volumes.JournalDeviceName) == 0 {
				t.Fatalf("expect journal device for service %s, service volumes %s", service, member.Volumes)
			}
		} else {
			if len(member.Volumes.PrimaryVolumeID) == 0 || len(member.Volumes.PrimaryDeviceName) == 0 ||
				len(member.Volumes.JournalVolumeID) != 0 || len(member.Volumes.JournalDeviceName) != 0 {
				t.Fatalf("not expect journal device for service %s, service volumes %s", service, member.Volumes)
			}
		}
	}
	// increase the firstIP for the next service
	firstIP = firstIP + taskCount

	// 2. device and service item exist
	idx++
	service = servicePrefix + strconv.Itoa(idx)
	dev, err = s.createDevice(ctx, cluster, service, "", "requuid")
	expectDev := "/dev/loop2"
	if requireJournalVolume {
		expectDev = "/dev/loop3"
	}
	if err != nil || dev != expectDev {
		t.Fatalf("createDevice error %s, expectDev %s got %s", err, expectDev, dev)
	}

	serviceItem := db.CreateService(cluster, service, "uuid"+service)
	err = dbIns.CreateService(ctx, serviceItem)
	if err != nil {
		t.Fatalf("CreateService error %s, serviceItem %s", err, serviceItem)
	}

	// update the ServiceName in CreateServiceRequest
	req.Service.ServiceName = service

	svcUUID, err = s.CreateService(ctx, req, domain, vpcID)
	if err != nil || svcUUID != "uuid"+service {
		t.Fatalf("CreateService error %s, cluster %s, service %s, taskCount %d, uuid %s %s",
			err, cluster, service, taskCount, svcUUID, "uuid"+service)
	}
	// get service attr to verify
	sattr, err = dbIns.GetServiceAttr(ctx, svcUUID)
	if err != nil {
		t.Fatalf("GetServiceAttr error %s, cluster %s, service %s", err, cluster, service)
	}
	if sattr.Volumes.PrimaryDeviceName != expectDev {
		t.Fatalf("expect primary device %s for service %s, service volumes %s", expectDev, service, sattr.Volumes)
	}
	if requireJournalVolume {
		expectDev = "/dev/loop4"
		if sattr.Volumes.JournalDeviceName != expectDev {
			t.Fatalf("expect journal device %s for service %s, service volumes %s", expectDev, service, sattr.Volumes)
		}
	} else {
		if len(sattr.Volumes.JournalDeviceName) != 0 {
			t.Fatalf("not expect journal device for service %s, service volumes %s", service, sattr.Volumes)
		}
	}
	// list service members to verify
	memberItems, err = dbIns.ListServiceMembers(ctx, svcUUID)
	if err != nil || len(memberItems) != taskCount {
		t.Fatalf("got %d, expect %d serviceMembers for service %s", len(memberItems), taskCount, service)
	}
	sort.Slice(memberItems, func(i, j int) bool {
		idx1, err := utils.GetServiceMemberIndex(memberItems[i].MemberName)
		if err != nil {
			t.Fatalf("GetServiceMemberIndex error %s, %s", err, memberItems[i])
		}
		idx2, err := utils.GetServiceMemberIndex(memberItems[j].MemberName)
		if err != nil {
			t.Fatalf("GetServiceMemberIndex error %s, %s", err, memberItems[j])
		}
		return idx1 < idx2
	})
	for i, member := range memberItems {
		name := utils.GenServiceMemberName(service, int64(i))
		if member.MemberName != name {
			t.Fatalf("expect member name %s, get %s", name, member)
		}
		if requireStaticIP {
			ip := ipPrefix + strconv.Itoa(firstIP+i)
			if member.StaticIP != ip {
				t.Fatalf("expect ip %s for member %s", ip, member)
			}
		}
		if requireJournalVolume {
			if len(member.Volumes.PrimaryVolumeID) == 0 || len(member.Volumes.PrimaryDeviceName) == 0 ||
				len(member.Volumes.JournalVolumeID) == 0 || len(member.Volumes.JournalDeviceName) == 0 {
				t.Fatalf("expect journal device for service %s, service volumes %s", service, member.Volumes)
			}
		} else {
			if len(member.Volumes.PrimaryVolumeID) == 0 || len(member.Volumes.PrimaryDeviceName) == 0 ||
				len(member.Volumes.JournalVolumeID) != 0 || len(member.Volumes.JournalDeviceName) != 0 {
				t.Fatalf("not expect journal device for service %s, service volumes %s", service, member.Volumes)
			}
		}
	}
	// increase the firstIP for the next service
	firstIP = firstIP + taskCount

	// 3. device and service item exist, the 3rd device, service attr is at creating
	idx++
	service = servicePrefix + strconv.Itoa(idx)
	dev, err = s.createDevice(ctx, cluster, service, "", "requuid")
	expectDev = "/dev/loop3"
	if requireJournalVolume {
		expectDev = "/dev/loop5"
	}
	if err != nil || dev != expectDev {
		t.Fatalf("createDevice error %s, expectDev %s, got %s", err, expectDev, dev)
	}

	serviceItem = db.CreateService(cluster, service, "uuid"+service)
	err = dbIns.CreateService(ctx, serviceItem)
	if err != nil {
		t.Fatalf("CreateService error %s, serviceItem %s", err, serviceItem)
	}

	hostedZoneID, err := dnsIns.GetOrCreateHostedZoneIDByName(ctx, domain, vpcID, region, true)
	if err != nil {
		t.Fatalf("GetOrCreateHostedZoneIDByName error %s, domain %s, vpc %s %s", err, domain, vpcID, region)
	}

	// update the ServiceName in CreateServiceRequest
	req.Service.ServiceName = service
	svols, err := s.createServiceVolumes(ctx, req)
	if err != nil {
		t.Fatalf("createServiceVolumes error %s", err)
	}
	if svols.PrimaryDeviceName != expectDev {
		t.Fatalf("expect device %s got %s", expectDev, svols.PrimaryDeviceName)
	}
	if requireJournalVolume && svols.JournalDeviceName != "/dev/loop6" {
		t.Fatalf("expect journal device /dev/loop6, get %s", svols.JournalDeviceName)
	}
	serviceAttr := db.CreateInitialServiceAttr("uuid"+service, int64(taskCount),
		cluster, service, *svols, registerDNS, domain, hostedZoneID, requireStaticIP, nil)
	err = dbIns.CreateServiceAttr(ctx, serviceAttr)
	if err != nil {
		t.Fatalf("CreateServiceAttr error %s, serviceAttr %s", err, serviceAttr)
	}

	svcUUID, err = s.CreateService(ctx, req, domain, vpcID)
	if err != nil || svcUUID != "uuid"+service {
		t.Fatalf("CreateService error %s, cluster %s, service %s, taskCount %d, uuid %s %s",
			err, cluster, service, taskCount, svcUUID, "uuid"+service)
	}
	// list service members to verify
	memberItems, err = dbIns.ListServiceMembers(ctx, svcUUID)
	if err != nil || len(memberItems) != taskCount {
		t.Fatalf("got %d, expect %d serviceMembers for service %s", len(memberItems), taskCount, service)
	}
	sort.Slice(memberItems, func(i, j int) bool {
		idx1, err := utils.GetServiceMemberIndex(memberItems[i].MemberName)
		if err != nil {
			t.Fatalf("GetServiceMemberIndex error %s, %s", err, memberItems[i])
		}
		idx2, err := utils.GetServiceMemberIndex(memberItems[j].MemberName)
		if err != nil {
			t.Fatalf("GetServiceMemberIndex error %s, %s", err, memberItems[j])
		}
		return idx1 < idx2
	})
	for i, member := range memberItems {
		name := utils.GenServiceMemberName(service, int64(i))
		if member.MemberName != name {
			t.Fatalf("expect member name %s, get %s", name, member)
		}
		if requireStaticIP {
			ip := ipPrefix + strconv.Itoa(firstIP+i)
			if member.StaticIP != ip {
				t.Fatalf("expect ip %s for member %s", ip, member)
			}
		}
	}
	// increase the firstIP for the next service
	firstIP = firstIP + taskCount

	// 4. device and service item exist, the 4th device, service attr is at creating, and one serviceMember created
	idx++
	service = servicePrefix + strconv.Itoa(idx)
	dev, err = s.createDevice(ctx, cluster, service, "", "requuid")
	expectDev = "/dev/loop4"
	if requireJournalVolume {
		expectDev = "/dev/loop7"
	}
	if err != nil || dev != expectDev {
		t.Fatalf("createDevice error %s, expectDev %s, got %s", err, expectDev, dev)
	}

	serviceItem = db.CreateService(cluster, service, "uuid"+service)
	err = dbIns.CreateService(ctx, serviceItem)
	if err != nil {
		t.Fatalf("CreateService error %s, serviceItem %s", err, serviceItem)
	}

	vols := common.ServiceVolumes{
		PrimaryDeviceName: dev,
		PrimaryVolume: common.ServiceVolume{
			VolumeType:   common.VolumeTypeGPSSD,
			VolumeSizeGB: volSize,
		},
	}
	if requireJournalVolume {
		journalDev, err := s.createDevice(ctx, cluster, service, expectDev, "requuid")
		if err != nil || journalDev != "/dev/loop8" {
			t.Fatalf("create journal device error %s, expectDev /dev/loop8, got %s", err, journalDev)
		}
		vols.JournalDeviceName = journalDev
		vols.JournalVolume = common.ServiceVolume{
			VolumeType:   common.VolumeTypeGPSSD,
			VolumeSizeGB: volSize,
			Iops:         1,
		}
	}
	serviceAttr = db.CreateInitialServiceAttr("uuid"+service, int64(taskCount),
		cluster, service, vols, registerDNS, domain, hostedZoneID, requireStaticIP, nil)
	err = dbIns.CreateServiceAttr(ctx, serviceAttr)
	if err != nil {
		t.Fatalf("CreateServiceAttr error %s, serviceAttr %s", err, serviceAttr)
	}

	memberName := utils.GenServiceMemberName(service, 0)
	cfgs, err := s.checkAndCreateConfigFile(ctx, "uuid"+service, memberName, replicaCfgs[0])
	if err != nil {
		t.Fatalf("checkAndCreateConfigFile error %s, serviceItem %s", err, serviceItem)
	}

	ip := common.DefaultHostIP
	if requireStaticIP {
		// create the next ip and assign to member
		assignedIPs := make(map[string]string)
		serviceip, err := s.createStaticIPsForZone(ctx, serviceAttr, assignedIPs, 1, az)
		if err != nil {
			t.Fatalf("createStaticIPsForZone error %s", err)
		}
		if len(serviceip) != 1 {
			t.Fatalf("expect 1 serviceip, get %d", len(serviceip))
		}
		ip = ipPrefix + strconv.Itoa(firstIP)
		if serviceip[0].StaticIP != ip {
			t.Fatalf("expect ip %s, get %s", ip, serviceip[0])
		}
	}

	_, err = s.createServiceMember(ctx, serviceAttr, az, memberName, ip, cfgs)
	if err != nil {
		t.Fatalf("createServiceServiceMember error %s, serviceItem %s", err, serviceItem)
	}

	if requireStaticIP {
		// create the next ip and not assign to member
		assignedIPs := make(map[string]string)
		assignedIPs[ip] = memberName
		serviceip, err := s.createStaticIPsForZone(ctx, serviceAttr, assignedIPs, 1, az)
		if err != nil {
			t.Fatalf("createStaticIPsForZone error %s", err)
		}
		if len(serviceip) != 1 {
			t.Fatalf("expect 1 serviceip, get %d", len(serviceip))
		}
		ip = ipPrefix + strconv.Itoa(firstIP+1)
		if serviceip[0].StaticIP != ip {
			t.Fatalf("expect ip %s, get %s", ip, serviceip[0])
		}
	}

	// update the ServiceName in CreateServiceRequest
	req.Service.ServiceName = service

	svcUUID, err = s.CreateService(ctx, req, domain, vpcID)
	if err != nil || svcUUID != "uuid"+service {
		t.Fatalf("CreateService error %s, cluster %s, service %s, taskCount %d, uuid %s %s",
			err, cluster, service, taskCount, svcUUID, "uuid"+service)
	}
	// get service attr to verify
	sattr, err = dbIns.GetServiceAttr(ctx, svcUUID)
	if err != nil {
		t.Fatalf("GetServiceAttr error %s, cluster %s, service %s", err, cluster, service)
	}
	if sattr.Volumes.PrimaryDeviceName != expectDev {
		t.Fatalf("expect primary device %s for service %s, service volumes %s", expectDev, service, sattr.Volumes)
	}
	if requireJournalVolume {
		expectDev = "/dev/loop8"
		if sattr.Volumes.JournalDeviceName != expectDev {
			t.Fatalf("expect journal device %s for service %s, service volumes %s", expectDev, service, sattr.Volumes)
		}
	} else {
		if len(sattr.Volumes.JournalDeviceName) != 0 {
			t.Fatalf("not expect journal device for service %s, service volumes %s", service, sattr.Volumes)
		}
	}
	// list service members to verify
	memberItems, err = dbIns.ListServiceMembers(ctx, svcUUID)
	if err != nil || len(memberItems) != taskCount {
		t.Fatalf("got %d, expect %d serviceMembers for service %s", len(memberItems), taskCount, service)
	}
	sort.Slice(memberItems, func(i, j int) bool {
		idx1, err := utils.GetServiceMemberIndex(memberItems[i].MemberName)
		if err != nil {
			t.Fatalf("GetServiceMemberIndex error %s, %s", err, memberItems[i])
		}
		idx2, err := utils.GetServiceMemberIndex(memberItems[j].MemberName)
		if err != nil {
			t.Fatalf("GetServiceMemberIndex error %s, %s", err, memberItems[j])
		}
		return idx1 < idx2
	})
	for i, member := range memberItems {
		name := utils.GenServiceMemberName(service, int64(i))
		if member.MemberName != name {
			t.Fatalf("expect member name %s, get %s", name, member)
		}
		if requireStaticIP {
			ip := ipPrefix + strconv.Itoa(firstIP+i)
			if member.StaticIP != ip {
				t.Fatalf("expect ip %s for member %s", ip, member)
			}
		}
		if requireJournalVolume {
			if len(member.Volumes.PrimaryVolumeID) == 0 || len(member.Volumes.PrimaryDeviceName) == 0 ||
				len(member.Volumes.JournalVolumeID) == 0 || len(member.Volumes.JournalDeviceName) == 0 {
				t.Fatalf("expect journal device for service %s, service volumes %s", service, member.Volumes)
			}
		} else {
			if len(member.Volumes.PrimaryVolumeID) == 0 || len(member.Volumes.PrimaryDeviceName) == 0 ||
				len(member.Volumes.JournalVolumeID) != 0 || len(member.Volumes.JournalDeviceName) != 0 {
				t.Fatalf("not expect journal device for service %s, service volumes %s", service, member.Volumes)
			}
		}
	}
	// increase the firstIP for the next service
	firstIP = firstIP + taskCount

}
