package dtssdk

import (
	"bytes"
	"fmt"
	"github.com/Atian-OE/DTSSDK_Golang/dtssdk/codec"
	"github.com/Atian-OE/DTSSDK_Golang/dtssdk/model"
	"github.com/Atian-OE/DTSSDK_Golang/dtssdk/utils"
	"github.com/kataras/iris/core/errors"
	"log"
	"net"
	"sync"
	"time"
)

type WaitPackStr struct {
	Key     model.MsgID
	Timeout int64 //毫秒
	Call    *func(model.MsgID, []byte, net.Conn, error)
}

type DTSSDKClient struct {
	sess                  *net.TCPConn
	connected             bool
	waitPackList          *sync.Map        //等待这个包回传
	waitPackTimeoutTicker *time.Ticker     //等待回传的回调 会在 3秒后 自动删除
	waitPackTimeoutOver   chan interface{} //关闭自动删除
	heartBeatTicker       *time.Ticker     //心跳包的发送
	heartBeatTickerOver   chan interface{} //关闭心跳

	reconnectTicker     *time.Ticker     //自动连接
	reconnectTickerOver chan interface{} //关闭自动连接
	ReconnectTimes      int

	addr                     string                                //地址
	connectedAction          func(string)                          //连接到服务器的回调
	disconnectedAction       func(string)                          //断开连接到服务器的回调
	timeoutAction            func(string)                          //连接超时回调
	_ZoneTempNotifyEnable    bool                                  //接收分区温度更新的通知
	_ZoneTempNotify          func(*model.ZoneTempNotify, error)    //分区温度更新
	_ZoneAlarmNotifyEnable   bool                                  //接收温度警报的通知
	_ZoneAlarmNotify         func(*model.ZoneAlarmNotify, error)   //分区警报通知
	_FiberStatusNotifyEnable bool                                  //接收设备状态改变的通知
	_FiberStatusNotify       func(*model.DeviceEventNotify, error) //设备状态通知
	_TempSignalNotifyEnable  bool                                  //接收设备温度信号的通知
	_TempSignalNotify        func(*model.TempSignalNotify, error)  //设备状态通知
}

func NewDTSClient(addr string) *DTSSDKClient {
	conn := &DTSSDKClient{}
	conn.init(addr)
	return conn
}

func (d *DTSSDKClient) init(addr string) {
	d.addr = addr
	d.waitPackList = new(sync.Map)

	d.waitPackTimeoutTicker = time.NewTicker(time.Millisecond * 500)
	d.waitPackTimeoutOver = make(chan interface{})
	d.heartBeatTicker = time.NewTicker(time.Second * 5)
	d.heartBeatTickerOver = make(chan interface{})
	d.reconnectTicker = time.NewTicker(time.Second * 10)
	d.reconnectTickerOver = make(chan interface{})

	go d.waitPackTimeout()
	go d.heartBeat()
	go d.reconnect()
}

func (d *DTSSDKClient) connect() {
	if d.connected {
		return
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:17083", d.addr), time.Second*3)
	if err != nil {
		if d.timeoutAction != nil {
			d.timeoutAction(d.addr)
		}
		return
	}
	tcpConn, ok := conn.(*net.TCPConn)
	if !ok {
		if d.timeoutAction != nil {
			d.timeoutAction(d.addr)
		}
		return
	}
	d.sess = tcpConn
	//禁用缓存
	_ = tcpConn.SetWriteBuffer(5000)
	_ = tcpConn.SetReadBuffer(5000)
	go d.clientHandle(tcpConn)
}

func (d *DTSSDKClient) reconnect() {
	d.connected = false
	d.connect()
	count := 0
	for {
		select {
		case <-d.reconnectTicker.C:
			if !d.connected {
				count += 1
				if d.ReconnectTimes == 0 {
					log.Println(fmt.Sprintf("正在无限尝试第[ %d ]次重新连接...", count))
					d.connect()
				} else {
					if count < d.ReconnectTimes {
						log.Println(fmt.Sprintf("正在尝试第[ %d ]次重新连接...", count))
						d.connect()
					} else {
						log.Println(fmt.Sprintf("第[ %d ]次重新连接失败,断开连接...", count))
						d.Close()
					}
				}
			}
		case <-d.reconnectTickerOver:
			return

		}
	}
}

//心跳
func (d *DTSSDKClient) heartBeat() {
	for {
		select {
		case <-d.heartBeatTicker.C:
			if d.connected {
				b, _ := codec.Encode(&model.HeartBeat{})
				_, _ = d.sess.Write(b)
			}

		case <-d.heartBeatTickerOver:
			return
		}
	}
}

//超时删除回调
func (d *DTSSDKClient) waitPackTimeout() {
	for {
		select {
		case <-d.waitPackTimeoutTicker.C:
			d.waitPackList.Range(func(key, value interface{}) bool {

				v := value.(*WaitPackStr)
				v.Timeout -= 500
				if v.Timeout <= 0 {
					go (*v.Call)(0, nil, nil, errors.New("callback timeout"))
					d.waitPackList.Delete(key)
				}
				return true
			})

		case <-d.waitPackTimeoutOver:
			return

		}
	}
}

func (d *DTSSDKClient) clientHandle(conn net.Conn) {
	d.tcpHandle(model.MsgID_ConnectID, nil, conn)
	defer func() {
		if conn != nil {
			d.tcpHandle(model.MsgID_DisconnectID, nil, conn)
			_ = conn.Close()
		}
	}()

	buf := make([]byte, 1024)
	var cache bytes.Buffer
	for {
		//cache_index:=0
		n, err := conn.Read(buf)
		//加上上一次的缓存
		//n=buf_index+n
		if err != nil {
			break
		}

		cache.Write(buf[:n])
		for {
			if d.unpack(&cache, conn) {
				break
			}
		}
	}

}

// true 处理完成 false 循环继续处理
func (d *DTSSDKClient) unpack(cache *bytes.Buffer, conn net.Conn) bool {
	if cache.Len() < 5 {
		return true
	}
	buf := cache.Bytes()
	pkgSize := utils.ByteToInt2(buf[:4])
	//长度不够
	if pkgSize > len(buf)-5 {
		return true
	}

	cmd := buf[4]
	d.tcpHandle(model.MsgID(cmd), buf[:pkgSize+5], conn)
	cache.Reset()
	cache.Write(buf[5+pkgSize:])

	return false
}

//这个包会由这个回调接受
func (d *DTSSDKClient) waitPack(msgId model.MsgID, call *func(model.MsgID, []byte, net.Conn, error)) {
	d.waitPackList.Store(call, &WaitPackStr{Key: msgId, Timeout: 10000, Call: call})
}

//删除这个回调
func (d *DTSSDKClient) deleteWaitPackFunc(call *func(model.MsgID, []byte, net.Conn, error)) {

	value, ok := d.waitPackList.Load(call)
	if ok {
		v := value.(*WaitPackStr)
		go (*v.Call)(0, nil, nil, errors.New("cancel callback"))
		d.waitPackList.Delete(call)
	}

}

//发送消息
func (d *DTSSDKClient) Send(msgObj interface{}) error {
	b, err := codec.Encode(msgObj)
	if err != nil {
		return err
	}
	if !d.connected {
		return errors.New("client not connected")
	}
	_, err = d.sess.Write(b)
	return err
}

//关闭
func (d *DTSSDKClient) Close() {

	d.reconnectTicker.Stop()
	d.reconnectTickerOver <- 0
	close(d.reconnectTickerOver)

	d.heartBeatTicker.Stop()
	d.heartBeatTickerOver <- 0
	close(d.heartBeatTickerOver)

	d.waitPackTimeoutTicker.Stop()
	d.waitPackTimeoutOver <- 0
	close(d.waitPackTimeoutOver)

	if d.sess != nil {
		_ = d.sess.Close()
	}

	d.sess = nil
}
