package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/icholy/digest"
	"github.com/k0kubun/pp/v3"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sync/errgroup"
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

type EddiStatus int

const (
	EddiStatusUnknown EddiStatus = iota
	EddiStatusPaused
	_
	EddiStatusDiverting
	_
	EddiStatusMaxTempReached
	EddiStatusStopped
)

var eddiStatuses = map[EddiStatus]string{
	EddiStatusUnknown:        "unknown",
	EddiStatusPaused:         "paused",
	EddiStatusDiverting:      "diverting",
	EddiStatusMaxTempReached: "max-temp-reached",
	EddiStatusStopped:        "stopped",
}

func (s EddiStatus) String() string {
	if v, ok := eddiStatuses[s]; ok {
		return v
	}
	return "unknown"
}

// see https://github.com/twonk/MyEnergi-App-Api/blob/23a718a1ec8b3dd09842f312a0d79b249733f19f/README.md?plain=1#L130
type Eddi struct {
	SerialNumber       uint64     `json:"sno"`   // Eddi Serial Number
	Date               string     `json:"dat"`   // date
	Time               string     `json:"tim"`   // time
	Ectp1              int        `json:"ectp1"` // physical CT connection 1 value
	Ectp2              int        `json:"ectp2"` // physical CT connection 2 value
	Ectp3              int        `json:"ectp3"` // physical CT connection 3 value
	Ectt1              string     `json:"ectt1"` // CT 1 name
	Ectt2              string     `json:"ectt2"` // CT 2 name
	Ectt3              string     `json:"ectt3"` // CT 3 name
	BoostMode          uint64     `json:"bsm"`   // Boost Mode - 1 if boosting
	RemainingBoostTime int        `json:"rbt"`   // If boosting, the remaining boost time in of seconds
	SupplyFrequency    float64    `json:"frq"`   // Supply Frequency
	SupplyVoltage      int        `json:"vol"`   // Voltage out (divided by 10)
	Gen                int        `json:"gen"`   //Generated Watts
	FirmwareVersion    string     `json:"fwv"`
	GridPower          int        `json:"grd"` // Current Watts from Grid (negative if sending to grid)
	Hno                int        `json:"hno"` // Currently active heater (1 or 2)
	Pha                int        `json:"pha"` // Phase number of phases?
	Status             EddiStatus `json:"sta"` // Status of Eddi 1=Paused 3=Diverting 5=Max Temp Reached 6=Stopped
	Ht1                string     `json:"ht1"` // Heater 1 name
	Ht2                string     `json:"ht2"` // Heater 2 name
	Tp1                int        `json:"tp1"` // Temperature probe 1 (in Celsius)
	Tp2                int        `json:"tp2"` // Temperature probe 2 (in Celsius)
	Pri                int        `json:"pri"` // Priority
	Cmt                int        `json:"cmt"` // Command Timer - counts 1 - 10 when command sent, then 254 - success, 253 - failure, 255 - never received any commands
	R1A                int        `json:"r1a"`
	R2A                int        `json:"r2a"`
	R1B                int        `json:"r1b"`
	R2B                int        `json:"r2b"`
	Che                float64    `json:"che"` // total kWh tranferred this session (today?)
	Dst                int        `json:"dst"` // Daylight Savings Time enabled

}

func collectDateTime(dateS, timeS string, m prometheus.Gauge) {
	t, err := time.Parse("02-01-2006 15:04:05", dateS+" "+timeS)
	if err != nil {
		log.Printf("failed to parse time: %v", err)
		return
	}
	m.Set(float64(t.UnixNano()) / 1e9)
}

func (e *Eddi) collect(m *metrics) {
	serial := strconv.FormatUint(e.SerialNumber, 10)
	model := "eddi"

	m.info.WithLabelValues(model, serial, e.FirmwareVersion).Set(1)

	for s := range eddiStatuses {
		value := 0.0
		if s == e.Status {
			value = 1.0
		}
		m.status.WithLabelValues(model, serial, s.String()).Set(value)
	}

	m.gridPower.WithLabelValues(model, serial).Set(float64(e.GridPower))
	m.supplyVoltage.WithLabelValues(model, serial).Set(float64(e.SupplyVoltage) / 10)
	m.supplyFreq.WithLabelValues(model, serial).Set(float64(e.SupplyFrequency))
	if e.Ectt1 == "Internal Load" {
		m.loadPower.WithLabelValues(model, serial).Set(float64(e.Ectp1))
	}
	if e.Ht1 != "None" {
		m.temperature.WithLabelValues(model, serial, e.Ht1).Set(float64(e.Tp1))
	}
	if e.Ht2 != "None" {
		m.temperature.WithLabelValues(model, serial, e.Ht2).Set(float64(e.Tp2))
	}
	collectDateTime(e.Date, e.Time, m.lastSeen.WithLabelValues(model, serial))
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

type ZappiStatus int

const (
	ZappiStatusUnknown ZappiStatus = iota
	ZappiStatusPaused
	_
	ZappiStatusCharging
	_
	ZappiStatusComplete
)

var zappiStatuses = map[ZappiStatus]string{
	ZappiStatusUnknown:  "unknown",
	ZappiStatusPaused:   "paused",
	ZappiStatusCharging: "charging",
	ZappiStatusComplete: "complete",
}

func (s ZappiStatus) String() string {
	if v, ok := zappiStatuses[s]; ok {
		return v
	}
	return "unknown"
}

// see https://github.com/twonk/MyEnergi-App-Api/blob/23a718a1ec8b3dd09842f312a0d79b249733f19f/README.md?plain=1#L170
type Zappi struct {
	SerialNumber            uint64          `json:"sno"`
	Date                    string          `json:"dat"`
	Time                    string          `json:"tim"`
	Ectp1                   int             `json:"ectp1"` // physical CT connection 1 value
	Ectp2                   int             `json:"ectp2"` // physical CT connection 2 value
	Ectp3                   int             `json:"ectp3"` // physical CT connection 3 value
	Ectp4                   int             `json:"ectp4"` // physical CT connection 4 value
	Ectt1                   string          `json:"ectt1"` // CT 1 name
	Ectt2                   string          `json:"ectt2"` // CT 2 name
	Ectt3                   string          `json:"ectt3"` // CT 3 name
	Ectt4                   string          `json:"ectt4"` // CT 4 name
	BoostMode               uint64          `json:"bsm"`
	Bst                     int             `json:"bst"`
	Cmt                     int             `json:"cmt"`
	Dst                     int             `json:"dst"`
	Div                     int             `json:"div"`
	FirmwareVersion         string          `json:"fwv"`
	GridPower               int             `json:"grd"`
	Pha                     int             `json:"pha"`
	Pri                     int             `json:"pri"`
	Status                  ZappiStatus     `json:"sta"`
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
	NewAppAvailable         bool            `json:"newAppAvailable"`
	NewBootloaderAvailable  bool            `json:"newBootloaderAvailable"`
	BeingTamperedWith       bool            `json:"beingTamperedWith"`
	BatteryDischargeEnabled bool            `json:"batteryDischargeEnabled"`
	Mgl                     int             `json:"mgl"`
	Sbh                     int             `json:"sbh"`
	Sbk                     int             `json:"sbk"`
}

func (z *Zappi) collect(m *metrics) {
	model := "zappi"
	serial := strconv.FormatUint(z.SerialNumber, 10)

	m.info.WithLabelValues(model, serial, z.FirmwareVersion).Set(1)

	for s := range zappiStatuses {
		value := 0.0
		if s == z.Status {
			value = 1.0
		}
		m.status.WithLabelValues(model, serial, s.String()).Set(value)
	}

	for s := range zappiModes {
		value := 0.0
		if s == z.ZappiMode {
			value = 1.0
		}
		m.zappiMode.WithLabelValues(model, serial, s.String()).Set(value)
	}

	for s := range connectorStatuses {
		value := 0.0
		if s == z.ConnectorStatus {
			value = 1.0
		}
		m.connectorStatus.WithLabelValues(model, serial, s.String()).Set(value)
	}

	m.gridPower.WithLabelValues(model, serial).Set(float64(z.GridPower))
	if z.Ectt1 == "Internal Load" {
		m.loadPower.WithLabelValues(model, serial).Set(float64(z.Ectp1))
	}

	m.supplyVoltage.WithLabelValues(model, serial).Set(float64(z.SupplyVoltage) / 10)
	m.supplyFreq.WithLabelValues(model, serial).Set(float64(z.SupplyFrequency))
	collectDateTime(z.Date, z.Time, m.lastSeen.WithLabelValues(model, serial))
}

func getEddiStatus(ctx context.Context, client *http.Client) ([]*Eddi, error) {
	req, err := http.NewRequest("GET", "https://s18.myenergi.net/cgi-jstatus-E", nil)
	if err != nil {
		return nil, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	var respStatus struct {
		Eddi []*Eddi `json:"eddi"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&respStatus); err != nil {
		return nil, err
	}

	return respStatus.Eddi, nil
}

func getZappiStatus(ctx context.Context, client *http.Client) ([]*Zappi, error) {
	req, err := http.NewRequest("GET", "https://s18.myenergi.net/cgi-jstatus-Z", nil)
	if err != nil {
		return nil, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	var respStatus struct {
		Zappi []*Zappi `json:"zappi"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&respStatus); err != nil {
		return nil, err
	}

	return respStatus.Zappi, nil
}

func newClient() *http.Client {
	return &http.Client{
		Transport: &digest.Transport{
			Username: os.Getenv("ZAPPI_SERIAL"),
			Password: os.Getenv("ZAPPI_API_KEY"),
		},
	}
}

func run(ctx context.Context) error {
	client := newClient()
	zappis, err := getZappiStatus(ctx, client)
	if err != nil {
		return err
	}

	for _, z := range zappis {
		log.Printf("serial: %d", z.SerialNumber)
		log.Printf("status: %s", z.Status)
		log.Printf("zappi mode: %s", z.ZappiMode)
		log.Printf("connector status: %s", z.ConnectorStatus)

		for _, line := range strings.Split(pp.Sprint(z), "\n") {
			log.Println(line)
		}
	}

	eddis, err := getEddiStatus(ctx, client)
	if err != nil {
		return err
	}

	for _, e := range eddis {
		log.Printf("serial: %d", e.SerialNumber)
		log.Printf("status: %s", e.Status)

		for _, line := range strings.Split(pp.Sprint(e), "\n") {
			log.Println(line)
		}
	}

	return nil
}

func newMetrics(reg prometheus.Registerer) *metrics {
	namespace := "myenergi"
	return &metrics{
		client: newClient(),

		info:            prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: "info"}, []string{"model", "serial", "firmware_version"}),
		status:          prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: "status"}, []string{"model", "serial", "status"}),
		zappiMode:       prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: "zappi_mode"}, []string{"model", "serial", "mode"}),
		connectorStatus: prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: "connector_status"}, []string{"model", "serial", "status"}),
		gridPower:       prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: "grid_power_watt"}, []string{"model", "serial"}),
		loadPower:       prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: "load_power_watt"}, []string{"model", "serial"}),
		supplyVoltage:   prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: "supply_voltage"}, []string{"model", "serial"}),
		supplyFreq:      prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: "supply_frequency_hz"}, []string{"model", "serial"}),
		temperature:     prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: "temperature_celsius"}, []string{"model", "serial", "sensor"}),
		lastSeen:        prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: "last_seen_unixtime"}, []string{"model", "serial"}),
	}
}

type metrics struct {
	client *http.Client

	info            *prometheus.GaugeVec
	status          *prometheus.GaugeVec
	zappiMode       *prometheus.GaugeVec
	connectorStatus *prometheus.GaugeVec
	gridPower       *prometheus.GaugeVec
	loadPower       *prometheus.GaugeVec
	supplyVoltage   *prometheus.GaugeVec
	supplyFreq      *prometheus.GaugeVec
	temperature     *prometheus.GaugeVec
	lastSeen        *prometheus.GaugeVec
}

func (m *metrics) Describe(ch chan<- *prometheus.Desc) {
	m.info.Describe(ch)
	m.status.Describe(ch)
	m.zappiMode.Describe(ch)
	m.connectorStatus.Describe(ch)
	m.gridPower.Describe(ch)
	m.loadPower.Describe(ch)
	m.supplyVoltage.Describe(ch)
	m.supplyFreq.Describe(ch)
	m.temperature.Describe(ch)
	m.lastSeen.Describe(ch)

}

func (m *metrics) Collect(ch chan<- prometheus.Metric) {
	g, ctx := errgroup.WithContext(context.Background())

	m.info.Reset()

	g.Go(func() error {
		zappis, err := getZappiStatus(ctx, m.client)
		if err != nil {
			return err
		}

		for _, z := range zappis {
			z.collect(m)
		}

		return nil
	})

	g.Go(func() error {
		eddis, err := getEddiStatus(ctx, m.client)
		if err != nil {
			return err
		}

		for _, e := range eddis {
			e.collect(m)
		}

		return nil
	})

	m.info.Collect(ch)
	m.status.Collect(ch)
	m.zappiMode.Collect(ch)
	m.connectorStatus.Collect(ch)
	m.gridPower.Collect(ch)
	m.loadPower.Collect(ch)
	m.supplyVoltage.Collect(ch)
	m.supplyFreq.Collect(ch)
	m.temperature.Collect(ch)
	m.lastSeen.Collect(ch)
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

	log.Printf("myenergi exporter listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
