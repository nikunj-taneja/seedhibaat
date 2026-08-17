package service

import (
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nikunj-taneja/seedhibaat/internal/store"
	"github.com/nikunj-taneja/seedhibaat/internal/workflow"
)

//go:embed dashboard.html
var dashboardHTML string

//go:embed dashboard.css
var dashboardCSS []byte

//go:embed dashboard.js
var dashboardJS []byte

var dashboardTemplate = template.Must(template.New("dashboard").Parse(dashboardHTML))

type dashboardCard struct {
	Label string
	Value string
	Note  string
	Tone  string
}

type dashboardFunnelStep struct {
	Label string
	Value int64
	Max   int64
}

type dashboardBar struct {
	X         int
	Width     int
	ClickX    int
	ClickW    int
	HitX      int
	HitW      int
	LabelX    int
	Y         int
	Height    int
	ClickY    int
	ClickH    int
	TopY      int
	Label     string
	Delivered int64
	Clicks    int64
	ShowLabel bool
}

type dashboardTick struct {
	Y     int
	Label string
}

type dashboardRow struct {
	Kind      string
	Name      string
	Template  string
	Accepted  int64
	Delivered int64
	ReadRate  string
	Clicks    int64
	CTR       string
	Converted int64
	Revenue   string
	Spend     string
	ROAS      string
	Failed    int64
}

type dashboardWorkflow struct {
	Name             string
	Version          int
	Trigger          string
	Steps            int
	ActiveRecipients int64
	PendingJobs      int64
	NextActivity     string
	Delivered        int64
	ReadRate         string
}

type dashboardView struct {
	Title             string
	Range             string
	RangeLabel        string
	FromLabel         string
	ToLabel           string
	Retrieved         string
	Cards             []dashboardCard
	Funnel            []dashboardFunnelStep
	Bars              []dashboardBar
	Ticks             []dashboardTick
	Rows              []dashboardRow
	Workflows         []dashboardWorkflow
	AttributionWindow string
	Revenue           string
	Conversions       int64
	ConversionRate    string
	OptOuts           int64
	Replies           int64
	Spend             string
	ROAS              string
	Empty             bool
}

func (s *HTTPServer) metricsAuthenticated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.Config.MetricsEnabled {
			http.NotFound(w, r)
			return
		}
		username, password, ok := r.BasicAuth()
		expected := sha256.Sum256([]byte(s.Config.MetricsUsername + "\x00" + s.Config.MetricsPassword))
		supplied := sha256.Sum256([]byte(username + "\x00" + password))
		if !ok || subtle.ConstantTimeCompare(supplied[:], expected[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="SeedhiBaat metrics", charset="UTF-8"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Cache-Control", "private, no-store")
		next.ServeHTTP(w, r)
	})
}

func (s *HTTPServer) dashboardStyles(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	_, _ = w.Write(dashboardCSS)
}

func (s *HTTPServer) dashboardScript(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	_, _ = w.Write(dashboardJS)
}

func (s *HTTPServer) dashboard(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	rangeName := r.URL.Query().Get("range")
	if rangeName == "" {
		rangeName = "30d"
	}
	var from time.Time
	var rangeLabel string
	switch rangeName {
	case "1d":
		from, rangeLabel = now.AddDate(0, 0, -1), "Last 24 hours"
	case "7d":
		from, rangeLabel = now.AddDate(0, 0, -7), "Last 7 days"
	case "90d":
		from, rangeLabel = now.AddDate(0, 0, -90), "Last 90 days"
	case "all":
		earliest, ok, err := s.Store.EarliestMessageTime(r.Context())
		if err != nil {
			http.Error(w, "metrics unavailable", http.StatusInternalServerError)
			return
		}
		if ok {
			from = earliest
		} else {
			from = now.AddDate(0, -1, 0)
		}
		rangeLabel = "All time"
	default:
		rangeName, from, rangeLabel = "30d", now.AddDate(0, 0, -30), "Last 30 days"
	}
	metrics, err := s.Store.Metrics(r.Context(), from, now)
	if err != nil {
		s.Logger.Error("dashboard metrics", "error", err)
		http.Error(w, "metrics unavailable", http.StatusInternalServerError)
		return
	}
	trendFrom := from
	if cutoff := now.AddDate(0, 0, -30); trendFrom.Before(cutoff) {
		trendFrom = cutoff
	}
	location, _ := time.LoadLocation(s.Config.ReportTimezone)
	daily, err := s.Store.DailyMetrics(r.Context(), trendFrom, now, location)
	if err != nil {
		s.Logger.Error("dashboard daily metrics", "error", err)
		http.Error(w, "metrics unavailable", http.StatusInternalServerError)
		return
	}
	breakdown, err := s.Store.PerformanceBreakdown(r.Context(), from, now)
	if err != nil {
		s.Logger.Error("dashboard breakdown", "error", err)
		http.Error(w, "metrics unavailable", http.StatusInternalServerError)
		return
	}
	activeWorkflows, err := s.Store.ActiveWorkflowDashboard(r.Context(), from, now)
	if err != nil {
		s.Logger.Error("dashboard active workflows", "error", err)
		http.Error(w, "metrics unavailable", http.StatusInternalServerError)
		return
	}
	view := dashboardView{
		Title:             "SeedhiBaat analytics",
		Range:             rangeName,
		RangeLabel:        rangeLabel,
		FromLabel:         from.In(location).Format("2 Jan 2006"),
		ToLabel:           now.In(location).Format("2 Jan 2006"),
		Retrieved:         now.In(location).Format("3:04 PM"),
		AttributionWindow: humanDuration(s.Config.AttributionWindow),
		Revenue:           formatRevenue(metrics.RevenueByCurrency),
		Conversions:       metrics.ConvertedRecipients,
		ConversionRate:    ratio(metrics.ConversionRate, metrics.DeliveredRecipients),
		OptOuts:           metrics.OptOuts,
		Replies:           metrics.Replies,
		Empty:             metrics.Attempted == 0,
	}
	var marketingDelivered int64
	for _, item := range breakdown {
		marketingDelivered += item.MarketingDelivered
	}
	spendMicros := marketingDelivered * s.Config.MarketingMessageCostMicros
	view.Spend = "—"
	view.ROAS = "—"
	if s.Config.MarketingMessageCostMicros > 0 {
		view.Spend = formatINRMicros(spendMicros)
		if revenue, ok := revenueForCurrency(metrics.RevenueByCurrency, "INR"); ok && spendMicros > 0 {
			view.ROAS = formatROAS(float64(revenue*10000) / float64(spendMicros))
		}
	}
	view.Cards = []dashboardCard{
		{Label: "Delivery rate", Value: ratio(metrics.DeliveryRate, metrics.Accepted), Note: comma(metrics.Delivered) + " delivered of " + comma(metrics.Accepted) + " accepted", Tone: "green"},
		{Label: "Read rate", Value: ratio(metrics.ObservedReadRate, metrics.Delivered), Note: comma(metrics.ObservedRead) + " Meta read receipts", Tone: "green"},
		{Label: "Unique CTR", Value: ratio(metrics.UniqueCTR, metrics.Delivered), Note: comma(metrics.UniqueClicks) + " unique clickers", Tone: "green"},
		{Label: "CVR", Value: ratio(metrics.ConversionRate, metrics.DeliveredRecipients), Note: comma(metrics.ConvertedRecipients) + " converted ÷ " + comma(metrics.DeliveredRecipients) + " delivered recipients", Tone: "green"},
		{Label: "Attributed revenue", Value: view.Revenue, Note: comma(metrics.ConvertedRecipients) + " converted recipients", Tone: "green"},
		{Label: "Estimated spend", Value: view.Spend, Note: comma(marketingDelivered) + " delivered marketing messages", Tone: "green"},
		{Label: "ROAS", Value: view.ROAS, Note: "Attributed revenue ÷ estimated spend", Tone: "green"},
	}
	maximum := metrics.Attempted
	if maximum < 1 {
		maximum = 1
	}
	view.Funnel = []dashboardFunnelStep{
		funnelStep("Attempted", metrics.Attempted, maximum),
		funnelStep("Accepted", metrics.Accepted, maximum),
		funnelStep("Sent", metrics.Sent, maximum),
		funnelStep("Delivered", metrics.Delivered, maximum),
		funnelStep("Read", metrics.ObservedRead, maximum),
		funnelStep("Unique click", metrics.UniqueClicks, maximum),
		funnelStep("Converted", metrics.ConvertedRecipients, maximum),
	}
	view.Bars, view.Ticks = makeChart(daily)
	for _, item := range breakdown {
		readRate := 0.0
		ctr := 0.0
		if item.Delivered > 0 {
			readRate = float64(item.ObservedRead) / float64(item.Delivered)
		}
		if item.DeliveredRecipients > 0 {
			ctr = float64(item.UniqueClicks) / float64(item.DeliveredRecipients)
		}
		revenue := "—"
		spend := "—"
		roas := "—"
		if item.RevenueMinor != 0 {
			revenue = formatSingleRevenue(item.Currencies, item.RevenueMinor)
		}
		rowSpendMicros := item.MarketingDelivered * s.Config.MarketingMessageCostMicros
		if s.Config.MarketingMessageCostMicros > 0 {
			spend = formatINRMicros(rowSpendMicros)
			if item.Currencies == "INR" && rowSpendMicros > 0 {
				roas = formatROAS(float64(item.RevenueMinor*10000) / float64(rowSpendMicros))
			}
		}
		view.Rows = append(view.Rows, dashboardRow{
			Kind:      item.Kind,
			Name:      item.Name,
			Template:  item.TemplateName,
			Accepted:  item.Accepted,
			Delivered: item.Delivered,
			ReadRate:  percent(readRate),
			Clicks:    item.UniqueClicks,
			CTR:       percent(ctr),
			Converted: item.ConvertedRecipients,
			Revenue:   revenue,
			Spend:     spend,
			ROAS:      roas,
			Failed:    item.Failed,
		})
	}
	for _, item := range activeWorkflows {
		loaded, err := workflow.Parse("database:"+item.Name, []byte(item.YAML))
		if err != nil {
			s.Logger.Error("dashboard workflow definition", "workflow", item.Name, "error", err)
			continue
		}
		readRate := 0.0
		if item.Delivered > 0 {
			readRate = float64(item.ObservedRead) / float64(item.Delivered)
		}
		nextActivity := "Waiting for trigger"
		if item.NextActivity.Valid {
			if next, err := time.Parse(time.RFC3339Nano, item.NextActivity.String); err == nil {
				nextActivity = next.In(location).Format("2 Jan, 3:04 PM")
			}
		}
		view.Workflows = append(view.Workflows, dashboardWorkflow{
			Name:             item.Name,
			Version:          item.Version,
			Trigger:          humanizeIdentifier(loaded.Definition.Trigger.Type),
			Steps:            len(loaded.Definition.Steps),
			ActiveRecipients: item.ActiveRecipients,
			PendingJobs:      item.PendingJobs,
			NextActivity:     nextActivity,
			Delivered:        item.Delivered,
			ReadRate:         percent(readRate),
		})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := dashboardTemplate.Execute(w, view); err != nil {
		s.Logger.Error("render dashboard", "error", err)
	}
}

func funnelStep(label string, value, maximum int64) dashboardFunnelStep {
	return dashboardFunnelStep{Label: label, Value: value, Max: maximum}
}

// axisStep rounds a raw value up to the next 1/2/5 x power of ten so gridline
// labels read as round numbers instead of arbitrary thirds of the peak.
func axisStep(value int64) int64 {
	if value <= 1 {
		return 1
	}
	magnitude := int64(1)
	for magnitude*10 <= value {
		magnitude *= 10
	}
	for _, multiple := range []int64{1, 2, 5} {
		if step := magnitude * multiple; step >= value {
			return step
		}
	}
	return magnitude * 10
}

func compactCount(value int64) string {
	if value < 1000 {
		return strconv.FormatInt(value, 10)
	}
	scaled := float64(value) / 1000
	if scaled >= 10 || value%1000 == 0 {
		return strconv.FormatInt(value/1000, 10) + "k"
	}
	return strconv.FormatFloat(scaled, 'f', 1, 64) + "k"
}

func makeChart(daily []store.DailyMetric) ([]dashboardBar, []dashboardTick) {
	if len(daily) == 0 {
		return nil, nil
	}
	var peak int64
	for _, day := range daily {
		if day.Delivered > peak {
			peak = day.Delivered
		}
		if day.UniqueClicks > peak {
			peak = day.UniqueClicks
		}
	}
	const (
		plotLeft   = 50
		plotRight  = 930
		plotWidth  = plotRight - plotLeft
		baseline   = 178
		plotHeight = 150
		bands      = 3
	)
	// Three equal bands keep the existing gridline positions, so the axis
	// maximum has to be a whole multiple of three round steps.
	step := axisStep((peak + bands - 1) / bands)
	axisMax := step * bands
	ticks := make([]dashboardTick, 0, bands+1)
	for band := 0; band <= bands; band++ {
		ticks = append(ticks, dashboardTick{
			Y:     baseline - band*(plotHeight/bands),
			Label: compactCount(step * int64(band)),
		})
	}

	slotWidth := float64(plotWidth) / float64(len(daily))
	// Bars grow with the slot so short ranges read as bars, not hairlines,
	// and long ranges stay separated.
	deliveredWidth := clampInt(int(slotWidth*0.34), 4, 18)
	clickWidth := clampInt(int(slotWidth*0.24), 3, 13)
	const pairGap = 2
	pairWidth := deliveredWidth + pairGap + clickWidth
	hitWidth := clampInt(int(slotWidth), pairWidth+6, 44)
	labelStep := (len(daily) + 7) / 8
	if labelStep < 1 {
		labelStep = 1
	}

	var bars []dashboardBar
	for index, day := range daily {
		height := int(float64(day.Delivered) / float64(axisMax) * plotHeight)
		clickHeight := int(float64(day.UniqueClicks) / float64(axisMax) * plotHeight)
		center := int(float64(plotLeft) + slotWidth*(float64(index)+0.5))
		left := center - pairWidth/2
		top := baseline - height
		if clickTop := baseline - clickHeight; clickTop < top {
			top = clickTop
		}
		bars = append(bars, dashboardBar{
			X:         left,
			Width:     deliveredWidth,
			ClickX:    left + deliveredWidth + pairGap,
			ClickW:    clickWidth,
			HitX:      center - hitWidth/2,
			HitW:      hitWidth,
			LabelX:    center,
			Y:         baseline - height,
			Height:    height,
			ClickY:    baseline - clickHeight,
			ClickH:    clickHeight,
			TopY:      top,
			Label:     day.Date,
			Delivered: day.Delivered,
			Clicks:    day.UniqueClicks,
			ShowLabel: index%labelStep == 0 || index == len(daily)-1,
		})
	}
	return bars, ticks
}

func clampInt(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

func percent(value float64) string {
	return strconv.FormatFloat(value*100, 'f', 1, 64) + "%"
}

// ratio renders a rate that has no denominator as "—" instead of 0.0%, so an
// absent measurement never reads as a measured zero.
func ratio(value float64, denominator int64) string {
	if denominator == 0 {
		return "—"
	}
	return percent(value)
}

func comma(value int64) string {
	text := strconv.FormatInt(value, 10)
	for index := len(text) - 3; index > 0; index -= 3 {
		text = text[:index] + "," + text[index:]
	}
	return text
}

func formatRevenue(values []store.CurrencyAmount) string {
	if len(values) == 0 {
		return "—"
	}
	if len(values) > 1 {
		return strconv.Itoa(len(values)) + " currencies"
	}
	return formatSingleRevenue(values[0].Currency, values[0].AmountMinor)
}

func formatSingleRevenue(currency string, minor int64) string {
	if strings.Contains(currency, ",") {
		return "Mixed currencies"
	}
	amount := float64(minor) / 100
	switch currency {
	case "INR":
		return "₹" + strconv.FormatFloat(amount, 'f', 2, 64)
	case "USD":
		return "$" + strconv.FormatFloat(amount, 'f', 2, 64)
	default:
		if currency == "" {
			return "—"
		}
		return currency + " " + strconv.FormatFloat(amount, 'f', 2, 64)
	}
}

func revenueForCurrency(values []store.CurrencyAmount, currency string) (int64, bool) {
	if len(values) != 1 || values[0].Currency != currency {
		return 0, false
	}
	return values[0].AmountMinor, true
}

func formatROAS(value float64) string {
	return strconv.FormatFloat(value, 'f', 2, 64) + "×"
}

func formatINRMicros(value int64) string {
	return "₹" + strconv.FormatFloat(float64(value)/1_000_000, 'f', 2, 64)
}

func humanDuration(value time.Duration) string {
	hours := int(value.Hours())
	if hours%24 == 0 {
		days := hours / 24
		return fmt.Sprintf("%d-day last-touch", days)
	}
	return value.String() + " last-touch"
}

func humanizeIdentifier(value string) string {
	return strings.Title(strings.ReplaceAll(value, "_", " "))
}
