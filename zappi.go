package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/icholy/digest"
	"github.com/k0kubun/pp/v3"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type ConnectorStatus string

const (
	ConnectorStatusEVDisconnected ConnectorStatus = "A"
	ConnectorStatusEVConnected    ConnectorStatus = "B1"
	ConnectorStatusEVWaiting      ConnectorStatus = "B2"
	ConnectorStatusReadyToCharge  ConnectorStatus = "C1"
	ConnectorStatusCharging       ConnectorStatus = "C2"
	ConnectorStatusFault          ConnectorStatus = "F"
)

var connectorStatuses = map[ConnectorStatus]string{
	ConnectorStatusEVDisconnected: "ev-disconnected",
	ConnectorStatusEVConnected:    "ev-connected",
	ConnectorStatusEVWaiting:      "ev-waiting",
	ConnectorStatusReadyToCharge:  "ready-to-charge",
	ConnectorStatusCharging:       "charging",
	ConnectorStatusFault:          "fault",
}

func (m ConnectorStatus) String() string {
	if s, ok := connectorStatuses[m]; ok {
		return s
	}
	return "unknown"
}

type ZappiMode int

const (
	ZappiModeUnknown ZappiMode = iota
	ZappiModeFast
	ZappiModeEco
	ZappiModeEcoPlus
	ZappiModeStopped
)

var zappiModes = map[ZappiMode]string{
	ZappiModeFast:    "fast",
	ZappiModeEco:     "eco",
	ZappiModeEcoPlus: "eco+",
	ZappiModeStopped: "stopped",
}

func (m ZappiMode) String() string {
	if s, ok := zappiModes[m]; ok {
		return s
	}
	return "unknown"
}

type Status int

const (
	StatusUnknown Status = iota
	StatusPaused
	_
	StatusCharging
	_
	StatusComplete
)

var statuses = map[Status]string{
	StatusUnknown:  "unknown",
	StatusPaused:   "paused",
	StatusCharging: "charging",
	StatusComplete: "complete",
}

func (s Status) String() string {
	if v, ok := statuses[s]; ok {
		return v
	}
	return "unknown"
}

// see https://github.com/twonk/MyEnergi-App-Api/blob/23a718a1ec8b3dd09842f312a0d79b249733f19f/README.md?plain=1#L170
type Zappi struct {
	SerialNumber            uint64          `json:"sno"`
	Date                    string          `json:"dat"`
	Timer                   string          `json:"tim"`
	Ectp2                   int             `json:"ectp2"`
	Ectt1                   string          `json:"ectt1"`
	Ectt2                   string          `json:"ectt2"`
	Ectt3                   string          `json:"ectt3"`
	Bsm                     int             `json:"bsm"`
	Bst                     int             `json:"bst"`
	Cmt                     int             `json:"cmt"`
	Dst                     int             `json:"dst"`
	Div                     int             `json:"div"`
	FirmwareVersion         string          `json:"fwv"`
	GridPower               int             `json:"grd"`
	Pha                     int             `json:"pha"`
	Pri                     int             `json:"pri"`
	Status                  Status          `json:"sta"`
	Tz                      int             `json:"tz"`
	SupplyFrequency         float64         `json:"frq"`
	SupplyVoltage           int             `json:"vol"`
	Bss                     int             `json:"bss"`
	Lck                     int             `json:"lck"`
	ConnectorStatus         ConnectorStatus `json:"pst"`
	ZappiMode               ZappiMode       `json:"zmo"`
	Pwm                     int             `json:"pwm"`
	Zs                      int             `json:"zs"`
	Rdc                     int             `json:"rdc"`
	Rrac                    int             `json:"rrac"`
	Zsh                     int             `json:"zsh"`
	Ectt4                   string          `json:"ectt4"`
	Ectt5                   string          `json:"ectt5"`
	Ectt6                   string          `json:"ectt6"`
	NewAppAvailable         bool            `json:"newAppAvailable"`
	NewBootloaderAvailable  bool            `json:"newBootloaderAvailable"`
	BeingTamperedWith       bool            `json:"beingTamperedWith"`
	BatteryDischargeEnabled bool            `json:"batteryDischargeEnabled"`
	Mgl                     int             `json:"mgl"`
	Sbh                     int             `json:"sbh"`
	Sbk                     int             `json:"sbk"`
}

func getStatus(ctx context.Context) (*Zappi, error) {
	req, err := http.NewRequest("GET", "https://s18.myenergi.net/cgi-jstatus-Z", nil)
	if err != nil {
		return nil, err
	}

	client := &http.Client{
		Transport: &digest.Transport{
			Username: os.Getenv("ZAPPI_SERIAL"),
			Password: os.Getenv("ZAPPI_API_KEY"),
		},
	}
	resp, err := client.Do(req)
	defer resp.Body.Close()

	var respStatus struct {
		Zappi []Zappi `json:"zappi"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&respStatus); err != nil {
		return nil, err
	}

	if c := len(respStatus.Zappi); c == 0 {
		return nil, errors.New("no zappi found")
	} else if c > 1 {
		return nil, errors.New("got more than a single zappi back")
	}

	return &respStatus.Zappi[0], nil

}

func run(ctx context.Context) error {
	status, err := getStatus(ctx)
	if err != nil {
		return err
	}
	log.Printf("serial: %d", status.SerialNumber)
	log.Printf("status: %s", status.Status)
	log.Printf("zappi mode: %s", status.ZappiMode)
	log.Printf("connector status: %s", status.ConnectorStatus)

	for _, line := range strings.Split(pp.Sprint(status), "\n") {
		log.Println(line)
	}
	return nil
}

func newMetrics(reg prometheus.Registerer) *metrics {
	namespace := "zappi"
	return &metrics{
		info:            prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: "info"}, []string{"serial", "firmware_version"}),
		status:          prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: "status"}, []string{"serial", "status"}),
		zappiMode:       prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: "mode"}, []string{"serial", "mode"}),
		connectorStatus: prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: "connector_status"}, []string{"serial", "status"}),
		gridPower:       prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: "grid_power_watt"}, []string{"serial"}),
		supplyVoltage:   prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: "supply_voltage"}, []string{"serial"}),
		supplyFreq:      prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: "supply_frequency_hz"}, []string{"serial"}),
	}
}

type metrics struct {
	info            *prometheus.GaugeVec
	status          *prometheus.GaugeVec
	zappiMode       *prometheus.GaugeVec
	connectorStatus *prometheus.GaugeVec
	gridPower       *prometheus.GaugeVec
	supplyVoltage   *prometheus.GaugeVec
	supplyFreq      *prometheus.GaugeVec
}

func (m *metrics) Describe(ch chan<- *prometheus.Desc) {
	m.info.Describe(ch)
	m.status.Describe(ch)
	m.zappiMode.Describe(ch)
	m.connectorStatus.Describe(ch)
	m.gridPower.Describe(ch)
	m.supplyVoltage.Describe(ch)
	m.supplyFreq.Describe(ch)
}

func (m *metrics) Collect(ch chan<- prometheus.Metric) {
	status, err := getStatus(context.Background())
	if err != nil {
		return
	}

	serial := strconv.FormatUint(status.SerialNumber, 10)

	m.info.Reset()
	m.info.WithLabelValues(serial, status.FirmwareVersion).Set(1)

	for s := range statuses {
		value := 0.0
		if s == status.Status {
			value = 1.0
		}
		m.status.WithLabelValues(serial, s.String()).Set(value)
	}

	for s := range zappiModes {
		value := 0.0
		if s == status.ZappiMode {
			value = 1.0
		}
		m.zappiMode.WithLabelValues(serial, s.String()).Set(value)
	}

	for s := range connectorStatuses {
		value := 0.0
		if s == status.ConnectorStatus {
			value = 1.0
		}
		m.connectorStatus.WithLabelValues(serial, s.String()).Set(value)
	}

	m.gridPower.WithLabelValues(serial).Set(float64(status.GridPower))
	m.supplyVoltage.WithLabelValues(serial).Set(float64(status.SupplyVoltage) / 10)
	m.supplyFreq.WithLabelValues(serial).Set(float64(status.SupplyFrequency))

	m.info.Collect(ch)
	m.status.Collect(ch)
	m.zappiMode.Collect(ch)
	m.connectorStatus.Collect(ch)
	m.gridPower.Collect(ch)
	m.supplyVoltage.Collect(ch)
	m.supplyFreq.Collect(ch)
}

func main() {
	var (
		addr = flag.String("listen-address", ":8080", "The address to listen on for HTTP requests.")
	)

	if err := run(context.Background()); err != nil {
		log.Fatal(err)
	}

	// Create a non-global registry.
	reg := prometheus.NewRegistry()

	// Create new metrics and register them using the custom registry.
	m := newMetrics(nil)
	// Add Go module build info.
	reg.MustRegister(collectors.NewBuildInfoCollector())
	reg.MustRegister(m)

	// Expose the registered metrics via HTTP.
	http.Handle("/metrics", promhttp.HandlerFor(
		reg,
		promhttp.HandlerOpts{
			// Opt into OpenMetrics to support exemplars.
			EnableOpenMetrics: true,
			// Pass custom registry
			Registry: reg,
		},
	))

	log.Printf("zappi exporter listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
