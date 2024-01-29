package main

import (
	"flag"
	"fmt"
	influx "github.com/influxdata/influxdb-client-go/v2"
	influxAPI "github.com/influxdata/influxdb-client-go/v2/api"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
	"io/ioutil"
	"net"
	"strconv"
	"time"
)

var (
	debug      bool
	configFile string
	configData ConfigType
)

type SiteType struct {
	Address string `yaml:"address"`
	Region  string `yaml:"region"`
	Site    string `yaml:"site"`
}
type ConfigType struct {
	Period       uint       `yaml:"period"`
	LocalSite    SiteType   `yaml:"localSite"`
	RemoteSites  []SiteType `yaml:"remoteSites"`
	InfluxURL    string     `yaml:"influxUrl"`
	Port         uint       `yaml:"port"`
	InfluxBucket string     `yaml:"influxBucket"`
	InfluxOrg    string     `yaml:"influxOrg"`
	InfluxToken  string     `yaml:"influxToken"`
}

type TimestampType struct {
	Received string
	Current  string
}

func init() {
	flag.BoolVar(&debug, "debug", false, "Use debug logging")
	flag.StringVar(&configFile, "config", "/etc/netcheck/config.yaml", "Config file")
}

func min(a int64, b int64) int64 {
	if a < b && a != 0 {
		return a
	}
	return b
}

func max(a int64, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func startUDPServer(port uint) {
	svc, err := net.ListenPacket("udp", fmt.Sprintf(":%d", port))
	if err != nil {
		log.Fatal("Error listening socket")
	}
	defer svc.Close()
	buf := make([]byte, 9000)
	for {
		n, addr, err := svc.ReadFrom(buf)
		if err != nil {
			log.Info("Error reading")
			continue
		}
		go serve(svc, addr, buf[:n])
	}

}

func serve(svc net.PacketConn, addr net.Addr, buf []byte) {
	log.WithFields(log.Fields{"Client": addr.String()}).Debug(string(buf))
	svc.WriteTo(buf, addr)
}

func readerFunc(c chan TimestampType, conn *net.UDPConn) {
	buf := make([]byte, 9000)
	n, _, err := conn.ReadFrom(buf)
	ct := time.Now().UnixNano()
	if err != nil {
		log.Debug("Socket closed")
		return
	}
	res := TimestampType{Received: string(buf[:n]), Current: fmt.Sprintf("%d", ct)}
	c <- res
}
func CheckSite(API influxAPI.WriteAPI, localSite SiteType, remoteSite SiteType, port uint) {
	var minRTT int64
	var maxRTT int64
	var avgRTT int64
	var ts string
	var timer *time.Timer
	var res TimestampType
	log.WithFields(log.Fields{"Region": remoteSite.Region, "Site": remoteSite.Site}).Debug(fmt.Sprintf("Checking %d", remoteSite.Address))
	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", remoteSite.Address, port))
	if err != nil {
		log.WithFields(log.Fields{"Region": remoteSite.Region, "Site": remoteSite.Site}).Debug(fmt.Sprintf("Failed to parse %d:%d", remoteSite.Address, port))
		return
	}
	svc, err := net.DialUDP("udp", nil, addr)

	c := make(chan TimestampType)
	minRTT = 0
	maxRTT = 0
	avgRTT = 0
	for i := 0; i <= 9; i++ {
		ts = strconv.FormatInt(time.Now().UnixNano(), 10)
		svc.Write([]byte(ts))
		timer = time.NewTimer(10 * time.Second)
		go readerFunc(c, svc)
		select {
		case res = <-c:
			log.WithFields(log.Fields{"Region": remoteSite.Region, "Site": remoteSite.Site}).Debug(fmt.Sprintf("Got response from %d", remoteSite.Address))
		case <-timer.C:
			log.WithFields(log.Fields{"Region": remoteSite.Region, "Site": remoteSite.Site}).Debug(fmt.Sprintf("Failed to get response from %d", remoteSite.Address))
		}
		if !timer.Stop() {
			svc.Close()
			log.WithFields(log.Fields{"Region": remoteSite.Region, "Site": remoteSite.Site}).Debug(fmt.Sprintf("Timeout on %d", remoteSite.Address))
			return
		}
		received, _ := strconv.ParseInt(res.Received, 10, 64)
		current, _ := strconv.ParseInt(res.Current, 10, 64)
		rtt := time.Unix(0, current).Sub(time.Unix(0, received)).Microseconds()
		minRTT = min(minRTT, rtt)
		maxRTT = max(maxRTT, rtt)
		avgRTT += rtt
		time.Sleep(time.Second)
	}
	avgRTT = int64(avgRTT / 10)
	log.WithFields(log.Fields{"Client": addr.String()}).Debug(fmt.Sprintf("RTT is %d microsec, Jitter is %d microsec", avgRTT, maxRTT-minRTT))
	p := influx.NewPoint("rtt", map[string]string{"region1": localSite.Region, "region2": remoteSite.Region, "site1": localSite.Site, "site2": remoteSite.Site}, map[string]interface{}{"avg": avgRTT, "jitter": maxRTT - minRTT}, time.Now())
	API.WritePoint(p)
}

func main() {
	flag.Parse()
	if debug {
		log.SetLevel(log.DebugLevel)
	}
	configData.RemoteSites = make([]SiteType, 0)
	cfg, err := ioutil.ReadFile(configFile)
	if err != nil {
		log.Fatal("Failed to open config file")
	}
	err = yaml.Unmarshal(cfg, &configData)
	if err != nil {
		log.Fatal("error parsing file %s", err)
	}
	duration, err := time.ParseDuration(fmt.Sprintf("%ds", configData.Period))
	if err != nil {
		log.Fatal("error parsing period %s", err)
	}
	go startUDPServer(configData.Port)
	client := influx.NewClient(configData.InfluxURL, configData.InfluxToken)
	writeAPI := client.WriteAPI(configData.InfluxOrg, configData.InfluxBucket)
	ticker := time.NewTicker(duration)
	defer ticker.Stop()
	if len(configData.RemoteSites) == 0 {
		done := make(chan bool)
		<-done
	} else {
		for {
			<-ticker.C
			for _, site := range configData.RemoteSites {
				CheckSite(writeAPI, configData.LocalSite, site, configData.Port)
			}
		}
	}
}
