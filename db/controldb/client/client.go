package controldbcli

import (
	"io"
	"sync"
	"time"

	"github.com/golang/glog"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	"github.com/openconnectio/openmanage/common"
	"github.com/openconnectio/openmanage/db"
	"github.com/openconnectio/openmanage/db/controldb"
	pb "github.com/openconnectio/openmanage/db/controldb/protocols"
	"github.com/openconnectio/openmanage/utils"
)

const (
	maxRetryCount           = 3
	sleepSecondsBeforeRetry = 2
)

// ControlDBCli implements db interface and talks to ControlDBServer
type ControlDBCli struct {
	// address is ip:port
	addr string

	cliLock *sync.Mutex
	cli     *pbclient
}

type pbclient struct {
	// whether the connection is good
	isConnGood bool
	conn       *grpc.ClientConn
	dbcli      pb.ControlDBServiceClient
}

func NewControlDBCli(address string) *ControlDBCli {
	c := &ControlDBCli{
		addr:    address,
		cliLock: &sync.Mutex{},
		cli:     &pbclient{isConnGood: false},
	}

	c.connect()
	return c
}

func (c *ControlDBCli) getCli() *pbclient {
	if c.cli.isConnGood {
		return c.cli
	}

	// the current cli.isConnGood is false, connect again
	return c.connect()
}

func (c *ControlDBCli) connect() *pbclient {
	c.cliLock.Lock()
	defer c.cliLock.Unlock()

	// checkk isConnGood again, as another request may hold the lock and set up the connection
	if c.cli.isConnGood {
		return c.cli
	}

	// TODO support tls
	conn, err := grpc.Dial(c.addr, grpc.WithInsecure())
	if err != nil {
		glog.Errorln("grpc dial error", err, "address", c.addr)
		return c.cli
	}

	cli := &pbclient{
		isConnGood: true,
		conn:       conn,
		dbcli:      pb.NewControlDBServiceClient(conn),
	}

	c.cli = cli
	return c.cli
}

func (c *ControlDBCli) markClientFailed(cli *pbclient) (isClientChanged bool) {
	c.cliLock.Lock()
	defer c.cliLock.Unlock()

	if !c.cli.isConnGood {
		// the current connection is marked as failed, no need to mark again
		glog.V(1).Infoln("the current connection is already marked as failed", c.cli, cli)
		return false
	}

	if c.cli != cli {
		// the current connection is good and the failed cli is not the same with the current cli.
		// this means some other request already reconnects to the server.
		glog.V(1).Infoln("the current connection", c.cli, "is good, the failed connection is", cli)
		return true
	}

	// the failed cli is the same with the current cli, mark it failed
	c.cli.isConnGood = false
	// close the connection
	c.cli.conn.Close()
	return false
}

func (c *ControlDBCli) markClientFailedAndSleep(cli *pbclient) {
	isClientChanged := c.markClientFailed(cli)
	if !isClientChanged {
		// the current cli is marked as failed, wait some time before retry
		time.Sleep(sleepSecondsBeforeRetry * time.Second)
	}
}

func (c *ControlDBCli) checkAndConvertError(err error) error {
	// grpc defines the error codes in /grpcsrc/codes/codes.go.
	// if server side returns the application-level error, grpc will return error with
	// code = codes.Unknown, desc = applicationError.Error(), see /grpcsrc/rpc_util/toRPCError()
	switch grpc.ErrorDesc(err) {
	case db.StrErrDBInternal:
		return db.ErrDBInternal
	case db.StrErrDBInvalidRequest:
		return db.ErrDBInvalidRequest
	case db.StrErrDBRecordNotFound:
		return db.ErrDBRecordNotFound
	case db.StrErrDBConditionalCheckFailed:
		return db.ErrDBConditionalCheckFailed
	}
	return err
}

func (c *ControlDBCli) CreateSystemTables() error {
	return nil
}

func (c *ControlDBCli) SystemTablesReady() (tableStatus string, ready bool, err error) {
	return db.TableStatusActive, true, nil
}

func (c *ControlDBCli) DeleteSystemTables() error {
	return nil
}

func (c *ControlDBCli) CreateDevice(dev *common.Device) error {
	requuid := utils.GenRequestUUID()

	ctx, cancel := context.WithCancel(context.Background())
	ctx = utils.NewRequestContext(ctx, requuid)
	// call cancel before return. This is to ensure any resource derived
	// from the context will be canceled.
	defer cancel()

	var err error
	pbdev := controldb.GenPbDevice(dev)
	for i := 0; i < maxRetryCount; i++ {
		cli := c.getCli()
		_, err = cli.dbcli.CreateDevice(ctx, pbdev)
		if err == nil {
			glog.Infoln("created device", pbdev, "requuid", requuid)
			return nil
		}

		// error
		glog.Errorln("CreateDevice error", err, "device", pbdev, "requuid", requuid)
		if grpc.Code(err) == codes.Unknown {
			// not grpc layer error code, directly return
			return c.checkAndConvertError(err)
		}
		// grpc error, retry it
		c.markClientFailedAndSleep(cli)
	}
	return err
}

func (c *ControlDBCli) GetDevice(clusterName string, deviceName string) (dev *common.Device, err error) {
	requuid := utils.GenRequestUUID()

	ctx, cancel := context.WithCancel(context.Background())
	ctx = utils.NewRequestContext(ctx, requuid)
	// call cancel before return. This is to ensure any resource derived
	// from the context will be canceled.
	defer cancel()

	key := &pb.DeviceKey{
		ClusterName: clusterName,
		DeviceName:  deviceName,
	}
	for i := 0; i < maxRetryCount; i++ {
		cli := c.getCli()
		pbdev, err := cli.dbcli.GetDevice(ctx, key)
		if err == nil {
			glog.Infoln("got device", pbdev, "requuid", requuid)
			return controldb.GenDbDevice(pbdev), nil
		}

		// error
		glog.Errorln("GetDevice error", err, key, "requuid", requuid)
		if grpc.Code(err) == codes.Unknown {
			// not grpc layer error code, directly return
			return nil, c.checkAndConvertError(err)
		}
		// grpc error, retry it
		c.markClientFailedAndSleep(cli)
	}
	return nil, err
}

func (c *ControlDBCli) DeleteDevice(clusterName string, deviceName string) error {
	requuid := utils.GenRequestUUID()

	ctx, cancel := context.WithCancel(context.Background())
	ctx = utils.NewRequestContext(ctx, requuid)
	// call cancel before return. This is to ensure any resource derived
	// from the context will be canceled.
	defer cancel()

	var err error
	key := &pb.DeviceKey{
		ClusterName: clusterName,
		DeviceName:  deviceName,
	}
	for i := 0; i < maxRetryCount; i++ {
		cli := c.getCli()
		_, err = cli.dbcli.DeleteDevice(ctx, key)
		if err == nil {
			glog.Infoln("deleted device", key, "requuid", requuid)
			return nil
		}

		glog.Errorln("DeleteDevice error", err, key, "requuid", requuid)
		if grpc.Code(err) == codes.Unknown {
			// not grpc layer error code, directly return
			return c.checkAndConvertError(err)
		}
		// grpc error, retry it
		c.markClientFailedAndSleep(cli)
	}
	return err
}

func (c *ControlDBCli) listDevices(clusterName string, cli *pbclient, ctx context.Context,
	req *pb.ListDeviceRequest, requuid string) (devs []*common.Device, err error) {
	stream, err := cli.dbcli.ListDevices(ctx, req)
	if err != nil {
		glog.Errorln("ListDevices error", err, "cluster", clusterName, "requuid", requuid)
		return nil, err
	}

	for {
		pbdev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			glog.Errorln("list one device error", err, "cluster", clusterName, "requuid", requuid)
			return nil, err
		}

		// get one device
		dev := controldb.GenDbDevice(pbdev)
		devs = append(devs, dev)
		glog.V(1).Infoln("list one device", dev, "total", len(devs), "requuid", requuid)
	}

	if len(devs) > 0 {
		glog.Infoln("list", len(devs), "devices, last device is", devs[len(devs)-1], "requuid", requuid)
	} else {
		glog.Infoln("cluster", clusterName, "has no devices, requuid", requuid)
	}
	return devs, nil
}

func (c *ControlDBCli) ListDevices(clusterName string) (devs []*common.Device, err error) {
	requuid := utils.GenRequestUUID()

	ctx, cancel := context.WithCancel(context.Background())
	ctx = utils.NewRequestContext(ctx, requuid)
	// call cancel before return. This is to ensure any resource derived
	// from the context will be canceled.
	defer cancel()

	req := &pb.ListDeviceRequest{
		ClusterName: clusterName,
	}
	for i := 0; i < maxRetryCount; i++ {
		cli := c.getCli()
		devs, err = c.listDevices(clusterName, cli, ctx, req, requuid)
		if err == nil {
			return devs, nil
		}

		glog.Errorln("ListDevices error", err, req, "requuid", requuid)
		if grpc.Code(err) == codes.Unknown {
			// not grpc layer error code, directly return
			return nil, c.checkAndConvertError(err)
		}
		// grpc error, retry it
		c.markClientFailedAndSleep(cli)
	}
	return nil, err
}

func (c *ControlDBCli) CreateService(svc *common.Service) error {
	requuid := utils.GenRequestUUID()

	ctx, cancel := context.WithCancel(context.Background())
	ctx = utils.NewRequestContext(ctx, requuid)
	// call cancel before return. This is to ensure any resource derived
	// from the context will be canceled.
	defer cancel()

	var err error
	pbsvc := controldb.GenPbService(svc)
	for i := 0; i < maxRetryCount; i++ {
		cli := c.getCli()
		_, err = cli.dbcli.CreateService(ctx, pbsvc)
		if err == nil {
			glog.Infoln("created service", pbsvc, "requuid", requuid)
			return nil
		}

		glog.Errorln("CreateService error", err, "service", pbsvc, "requuid", requuid)
		if grpc.Code(err) == codes.Unknown {
			// not grpc layer error code, directly return
			return c.checkAndConvertError(err)
		}
		// grpc error, retry it
		c.markClientFailedAndSleep(cli)
	}
	return err
}

func (c *ControlDBCli) GetService(clusterName string, serviceName string) (svc *common.Service, err error) {
	requuid := utils.GenRequestUUID()

	ctx, cancel := context.WithCancel(context.Background())
	ctx = utils.NewRequestContext(ctx, requuid)
	// call cancel before return. This is to ensure any resource derived
	// from the context will be canceled.
	defer cancel()

	key := &pb.ServiceKey{
		ClusterName: clusterName,
		ServiceName: serviceName,
	}
	for i := 0; i < maxRetryCount; i++ {
		cli := c.getCli()
		pbsvc, err := cli.dbcli.GetService(ctx, key)
		if err == nil {
			glog.Infoln("get service", pbsvc, "requuid", requuid)
			return controldb.GenDbService(pbsvc), nil
		}

		glog.Errorln("GetService error", err, "key", key, "requuid", requuid)
		if grpc.Code(err) == codes.Unknown {
			// not grpc layer error code, directly return
			return nil, c.checkAndConvertError(err)
		}
		// grpc error, retry it
		c.markClientFailedAndSleep(cli)
	}
	return nil, err
}

func (c *ControlDBCli) DeleteService(clusterName string, serviceName string) error {
	requuid := utils.GenRequestUUID()

	ctx, cancel := context.WithCancel(context.Background())
	ctx = utils.NewRequestContext(ctx, requuid)
	// call cancel before return. This is to ensure any resource derived
	// from the context will be canceled.
	defer cancel()

	var err error
	key := &pb.ServiceKey{
		ClusterName: clusterName,
		ServiceName: serviceName,
	}
	for i := 0; i < maxRetryCount; i++ {
		cli := c.getCli()
		pbsvc, err := cli.dbcli.DeleteService(ctx, key)
		if err == nil {
			glog.Infoln("delete service", pbsvc, "requuid", requuid)
			return nil
		}

		glog.Errorln("DeleteService error", err, "key", key, "requuid", requuid)
		if grpc.Code(err) == codes.Unknown {
			// not grpc layer error code, directly return
			return c.checkAndConvertError(err)
		}
		// grpc error, retry it
		c.markClientFailedAndSleep(cli)
	}
	return err
}

func (c *ControlDBCli) listServices(clusterName string, cli *pbclient, ctx context.Context,
	req *pb.ListServiceRequest, requuid string) (svcs []*common.Service, err error) {
	stream, err := cli.dbcli.ListServices(ctx, req)
	if err != nil {
		glog.Errorln("ListServices error", err, "cluster", clusterName, "requuid", requuid)
		return nil, err
	}

	for {
		pbsvc, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			glog.Errorln("list one service error", err, "cluster", clusterName, "requuid", requuid)
			return nil, err
		}

		// get one service
		svc := controldb.GenDbService(pbsvc)
		svcs = append(svcs, svc)
		glog.V(1).Infoln("list one service", svc, "total", len(svcs), "requuid", requuid)
	}

	if len(svcs) > 0 {
		glog.Infoln("list", len(svcs), "services, last service is", svcs[len(svcs)-1], "requuid", requuid)
	} else {
		glog.Infoln("cluster", clusterName, "has no service, requuid", requuid)
	}
	return svcs, nil
}

func (c *ControlDBCli) ListServices(clusterName string) (svcs []*common.Service, err error) {
	requuid := utils.GenRequestUUID()

	ctx, cancel := context.WithCancel(context.Background())
	ctx = utils.NewRequestContext(ctx, requuid)
	// call cancel before return. This is to ensure any resource derived
	// from the context will be canceled.
	defer cancel()

	req := &pb.ListServiceRequest{
		ClusterName: clusterName,
	}

	for i := 0; i < maxRetryCount; i++ {
		cli := c.getCli()
		svcs, err = c.listServices(clusterName, cli, ctx, req, requuid)
		if err == nil {
			return svcs, nil
		}

		glog.Errorln("ListServices error", err, "cluster", clusterName, "requuid", requuid)
		if grpc.Code(err) == codes.Unknown {
			// not grpc layer error code, directly return
			return nil, c.checkAndConvertError(err)
		}
		// grpc error, retry it
		c.markClientFailedAndSleep(cli)
	}
	return nil, err
}

func (c *ControlDBCli) CreateServiceAttr(attr *common.ServiceAttr) error {
	requuid := utils.GenRequestUUID()

	ctx, cancel := context.WithCancel(context.Background())
	ctx = utils.NewRequestContext(ctx, requuid)
	// call cancel before return. This is to ensure any resource derived
	// from the context will be canceled.
	defer cancel()

	var err error
	pbattr := controldb.GenPbServiceAttr(attr)
	for i := 0; i < maxRetryCount; i++ {
		cli := c.getCli()
		_, err = cli.dbcli.CreateServiceAttr(ctx, pbattr)
		if err == nil {
			glog.Infoln("created service attr", pbattr, "requuid", requuid)
			return nil
		}

		glog.Errorln("CreateServiceAttr error", err, "serviceAttr", pbattr, "requuid", requuid)
		if grpc.Code(err) == codes.Unknown {
			// not grpc layer error code, directly return
			return c.checkAndConvertError(err)
		}
		// grpc error, retry it
		c.markClientFailedAndSleep(cli)
	}
	return err
}

func (c *ControlDBCli) UpdateServiceAttr(oldAttr *common.ServiceAttr, newAttr *common.ServiceAttr) error {
	requuid := utils.GenRequestUUID()

	ctx, cancel := context.WithCancel(context.Background())
	ctx = utils.NewRequestContext(ctx, requuid)
	// call cancel before return. This is to ensure any resource derived
	// from the context will be canceled.
	defer cancel()

	var err error
	req := &pb.UpdateServiceAttrRequest{
		OldAttr: controldb.GenPbServiceAttr(oldAttr),
		NewAttr: controldb.GenPbServiceAttr(newAttr),
	}
	for i := 0; i < maxRetryCount; i++ {
		cli := c.getCli()
		_, err = cli.dbcli.UpdateServiceAttr(ctx, req)
		if err == nil {
			glog.Infoln("UpdateServiceAttr from", oldAttr, "to", newAttr, "requuid", requuid)
			return nil
		}

		glog.Errorln("UpdateServiceAttr error", err, "old attr", oldAttr, "requuid", requuid)
		if grpc.Code(err) == codes.Unknown {
			// not grpc layer error code, directly return
			return c.checkAndConvertError(err)
		}
		// grpc error, retry it
		c.markClientFailedAndSleep(cli)
	}
	return err
}

func (c *ControlDBCli) GetServiceAttr(serviceUUID string) (attr *common.ServiceAttr, err error) {
	requuid := utils.GenRequestUUID()

	ctx, cancel := context.WithCancel(context.Background())
	ctx = utils.NewRequestContext(ctx, requuid)
	// call cancel before return. This is to ensure any resource derived
	// from the context will be canceled.
	defer cancel()

	key := &pb.ServiceAttrKey{
		ServiceUUID: serviceUUID,
	}
	for i := 0; i < maxRetryCount; i++ {
		cli := c.getCli()
		pbAttr, err := cli.dbcli.GetServiceAttr(ctx, key)
		if err == nil {
			glog.Infoln("get service attr", pbAttr, "requuid", requuid)
			return controldb.GenDbServiceAttr(pbAttr), nil
		}

		glog.Errorln("GetServiceAttr error", err, "service", serviceUUID, "requuid", requuid)
		if grpc.Code(err) == codes.Unknown {
			// not grpc layer error code, directly return
			return nil, c.checkAndConvertError(err)
		}
		// grpc error, retry it
		c.markClientFailedAndSleep(cli)
	}
	return nil, err
}

func (c *ControlDBCli) DeleteServiceAttr(serviceUUID string) error {
	requuid := utils.GenRequestUUID()

	ctx, cancel := context.WithCancel(context.Background())
	ctx = utils.NewRequestContext(ctx, requuid)
	// call cancel before return. This is to ensure any resource derived
	// from the context will be canceled.
	defer cancel()

	var err error
	key := &pb.ServiceAttrKey{
		ServiceUUID: serviceUUID,
	}
	for i := 0; i < maxRetryCount; i++ {
		cli := c.getCli()
		pbAttr, err := cli.dbcli.DeleteServiceAttr(ctx, key)
		if err == nil {
			glog.Infoln("delete service attr", pbAttr, "requuid", requuid)
			return nil
		}

		glog.Errorln("DeleteServiceAttr error", err, "service", serviceUUID, "requuid", requuid)
		if grpc.Code(err) == codes.Unknown {
			// not grpc layer error code, directly return
			return c.checkAndConvertError(err)
		}
		// grpc error, retry it
		c.markClientFailedAndSleep(cli)
	}
	return err
}

func (c *ControlDBCli) CreateVolume(vol *common.Volume) error {
	requuid := utils.GenRequestUUID()

	ctx, cancel := context.WithCancel(context.Background())
	ctx = utils.NewRequestContext(ctx, requuid)
	// call cancel before return. This is to ensure any resource derived
	// from the context will be canceled.
	defer cancel()

	var err error
	pbvol := controldb.GenPbVolume(vol)
	for i := 0; i < maxRetryCount; i++ {
		cli := c.getCli()
		_, err = cli.dbcli.CreateVolume(ctx, pbvol)
		if err == nil {
			glog.Infoln("created volume", pbvol, "requuid", requuid)
			return nil
		}

		glog.Errorln("CreateVolume error", err, "volume", pbvol, "requuid", requuid)
		if grpc.Code(err) == codes.Unknown {
			// not grpc layer error code, directly return
			return c.checkAndConvertError(err)
		}
		// grpc error, retry it
		c.markClientFailedAndSleep(cli)
	}
	return err
}

func (c *ControlDBCli) UpdateVolume(oldVol *common.Volume, newVol *common.Volume) error {
	requuid := utils.GenRequestUUID()

	ctx, cancel := context.WithCancel(context.Background())
	ctx = utils.NewRequestContext(ctx, requuid)
	// call cancel before return. This is to ensure any resource derived
	// from the context will be canceled.
	defer cancel()

	var err error
	req := &pb.UpdateVolumeRequest{
		OldVol: controldb.GenPbVolume(oldVol),
		NewVol: controldb.GenPbVolume(newVol),
	}
	for i := 0; i < maxRetryCount; i++ {
		cli := c.getCli()
		_, err = cli.dbcli.UpdateVolume(ctx, req)
		if err == nil {
			glog.Infoln("UpdateVolume from", oldVol, "to", newVol, "requuid", requuid)
			return nil
		}

		glog.Errorln("UpdateVolume error", err, "old volume", oldVol, "requuid", requuid)
		if grpc.Code(err) == codes.Unknown {
			// not grpc layer error code, directly return
			return c.checkAndConvertError(err)
		}
		// grpc error, retry it
		c.markClientFailedAndSleep(cli)
	}
	return err
}

func (c *ControlDBCli) GetVolume(serviceUUID string, volumeID string) (vol *common.Volume, err error) {
	requuid := utils.GenRequestUUID()

	ctx, cancel := context.WithCancel(context.Background())
	ctx = utils.NewRequestContext(ctx, requuid)
	// call cancel before return. This is to ensure any resource derived
	// from the context will be canceled.
	defer cancel()

	key := &pb.VolumeKey{
		ServiceUUID: serviceUUID,
		VolumeID:    volumeID,
	}
	for i := 0; i < maxRetryCount; i++ {
		cli := c.getCli()
		pbvol, err := cli.dbcli.GetVolume(ctx, key)
		if err == nil {
			glog.Infoln("get volume", pbvol, "requuid", requuid)
			return controldb.GenDbVolume(pbvol), nil
		}

		glog.Errorln("GetVolume error", err, "key", key, "requuid", requuid)
		if grpc.Code(err) == codes.Unknown {
			// not grpc layer error code, directly return
			return nil, c.checkAndConvertError(err)
		}
		// grpc error, retry it
		c.markClientFailedAndSleep(cli)
	}
	return nil, err
}

func (c *ControlDBCli) DeleteVolume(serviceUUID string, volumeID string) error {
	requuid := utils.GenRequestUUID()

	ctx, cancel := context.WithCancel(context.Background())
	ctx = utils.NewRequestContext(ctx, requuid)
	// call cancel before return. This is to ensure any resource derived
	// from the context will be canceled.
	defer cancel()

	var err error
	key := &pb.VolumeKey{
		ServiceUUID: serviceUUID,
		VolumeID:    volumeID,
	}
	for i := 0; i < maxRetryCount; i++ {
		cli := c.getCli()
		pbvol, err := cli.dbcli.DeleteVolume(ctx, key)
		if err == nil {
			glog.Infoln("delete volume", pbvol, "requuid", requuid)
			return nil
		}

		glog.Errorln("DeleteVolume error", err, "key", key, "requuid", requuid)
		if grpc.Code(err) == codes.Unknown {
			// not grpc layer error code, directly return
			return c.checkAndConvertError(err)
		}
		// grpc error, retry it
		c.markClientFailedAndSleep(cli)
	}
	return err
}

func (c *ControlDBCli) listVolumes(serviceUUID string, cli *pbclient, ctx context.Context,
	req *pb.ListVolumeRequest, requuid string) (vols []*common.Volume, err error) {
	stream, err := cli.dbcli.ListVolumes(ctx, req)
	if err != nil {
		glog.Errorln("ListVolumes error", err, "serviceUUID", serviceUUID, "requuid", requuid)
		return nil, err
	}

	for {
		pbvol, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			glog.Errorln("list one volume error", err, "serviceUUID", serviceUUID, "requuid", requuid)
			return nil, err
		}

		vol := controldb.GenDbVolume(pbvol)
		vols = append(vols, vol)
		glog.V(1).Infoln("list one volume", vol, "total", len(vols), "requuid", requuid)
	}

	if len(vols) > 0 {
		glog.Infoln("list", len(vols), "volumes, last volume is", vols[len(vols)-1], "requuid", requuid)
	} else {
		glog.Infoln("service has no volume", serviceUUID, "requuid", requuid)
	}
	return vols, nil
}

func (c *ControlDBCli) ListVolumes(serviceUUID string) (vols []*common.Volume, err error) {
	requuid := utils.GenRequestUUID()

	ctx, cancel := context.WithCancel(context.Background())
	ctx = utils.NewRequestContext(ctx, requuid)
	// call cancel before return. This is to ensure any resource derived
	// from the context will be canceled.
	defer cancel()

	req := &pb.ListVolumeRequest{
		ServiceUUID: serviceUUID,
	}
	for i := 0; i < maxRetryCount; i++ {
		cli := c.getCli()
		vols, err = c.listVolumes(serviceUUID, cli, ctx, req, requuid)
		if err == nil {
			return vols, nil
		}

		glog.Errorln("ListVolumes error", err, "serviceUUID", serviceUUID, "requuid", requuid)
		if grpc.Code(err) == codes.Unknown {
			// not grpc layer error code, directly return
			return nil, c.checkAndConvertError(err)
		}
		// grpc error, retry it
		c.markClientFailedAndSleep(cli)
	}
	return nil, err
}

func (c *ControlDBCli) CreateConfigFile(cfg *common.ConfigFile) error {
	requuid := utils.GenRequestUUID()

	ctx, cancel := context.WithCancel(context.Background())
	ctx = utils.NewRequestContext(ctx, requuid)
	// call cancel before return. This is to ensure any resource derived
	// from the context will be canceled.
	defer cancel()

	var err error
	pbcfg := controldb.GenPbConfigFile(cfg)
	for i := 0; i < maxRetryCount; i++ {
		cli := c.getCli()
		_, err = cli.dbcli.CreateConfigFile(ctx, pbcfg)
		if err == nil {
			glog.Infoln("created config file", pbcfg, "requuid", requuid)
			return nil
		}

		glog.Errorln("CreateConfigFile error", err, "config file", pbcfg, "requuid", requuid)
		if grpc.Code(err) == codes.Unknown {
			// not grpc layer error code, directly return
			return c.checkAndConvertError(err)
		}
		// grpc error, retry it
		c.markClientFailedAndSleep(cli)
	}
	return err
}

func (c *ControlDBCli) GetConfigFile(serviceUUID string, fileID string) (cfg *common.ConfigFile, err error) {
	requuid := utils.GenRequestUUID()

	ctx, cancel := context.WithCancel(context.Background())
	ctx = utils.NewRequestContext(ctx, requuid)
	// call cancel before return. This is to ensure any resource derived
	// from the context will be canceled.
	defer cancel()

	key := &pb.ConfigFileKey{
		ServiceUUID: serviceUUID,
		FileID:      fileID,
	}
	for i := 0; i < maxRetryCount; i++ {
		cli := c.getCli()
		pbcfg, err := cli.dbcli.GetConfigFile(ctx, key)
		if err == nil {
			glog.Infoln("get config file", pbcfg, "requuid", requuid)
			return controldb.GenDbConfigFile(pbcfg), nil
		}

		glog.Errorln("GetConfigFile error", err, "key", key, "requuid", requuid)
		if grpc.Code(err) == codes.Unknown {
			// not grpc layer error code, directly return
			return nil, c.checkAndConvertError(err)
		}
		// grpc error, retry it
		c.markClientFailedAndSleep(cli)
	}
	return nil, err
}

func (c *ControlDBCli) DeleteConfigFile(serviceUUID string, fileID string) error {
	requuid := utils.GenRequestUUID()

	ctx, cancel := context.WithCancel(context.Background())
	ctx = utils.NewRequestContext(ctx, requuid)
	// call cancel before return. This is to ensure any resource derived
	// from the context will be canceled.
	defer cancel()

	var err error
	key := &pb.ConfigFileKey{
		ServiceUUID: serviceUUID,
		FileID:      fileID,
	}
	for i := 0; i < maxRetryCount; i++ {
		cli := c.getCli()
		pbcfg, err := cli.dbcli.DeleteConfigFile(ctx, key)
		if err == nil {
			glog.Infoln("delete config file", pbcfg, "requuid", requuid)
			return nil
		}

		glog.Errorln("DeleteConfigFile error", err, "key", key, "requuid", requuid)
		if grpc.Code(err) == codes.Unknown {
			// not grpc layer error code, directly return
			return c.checkAndConvertError(err)
		}
		// grpc error, retry it
		c.markClientFailedAndSleep(cli)
	}
	return err
}
