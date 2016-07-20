package conf // import "bosun.org/cmd/bosun/conf"

import (
	"bytes"
	"fmt"
	"hash/fnv"
	"net/mail"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/influxdata/influxdb/client"

	"bosun.org/cmd/bosun/expr"
	"bosun.org/cmd/bosun/expr/parse"
	"bosun.org/graphite"
	"bosun.org/models"
	"bosun.org/opentsdb"

	htemplate "html/template"
	ttemplate "text/template"

	"bosun.org/slog"
)

type SystemConfProvider interface {
	GetHTTPListen() string
	GetRelayListen() string

	GetSMTPHost() string
	GetSMTPUsername() string // SMTP username
	GetSMTPPassword() string // SMTP password
	GetPing() bool
	GetPingDuration() time.Duration
	GetEmailFrom() string
	GetLedisDir() string
	GetLedisBindAddr() string
	GetRedisHost() string
	GetRedisDb() int
	GetRedisPassword() string
	GetTimeAndDate() []int
	GetSearchSince() opentsdb.Duration

	GetCheckFrequency() time.Duration
	GetDefaultRunEvery() int
	GetUnknownThreshold() int
	GetMinGroupSize() int

	GetShortURLKey() string
	GetInternetProxy() string

	GetRuleFilePath() string
	GetCommandHookPath() string

	SetTSDBHost(tsdbHost string)
	GetTSDBHost() string

	GetLogstashElasticHosts() expr.LogstashElasticHosts
	GetAnnotateElasticHosts() expr.ElasticHosts
	GetAnnotateIndex() string

	// Contexts , not sure these should be in conf but leaving them there for now
	GetTSDBContext() opentsdb.Context
	GetGraphiteContext() graphite.Context
	GetInfluxContext() client.Config
	GetLogstashContext() expr.LogstashElasticHosts
	GetElasticContext() expr.ElasticHosts
	AnnotateEnabled() bool

	MakeLink(string, *url.Values) string
	EnabledBackends() EnabledBackends
}

func ValidateSystemConf(sc SystemConfProvider) error {
	if sc.GetSMTPHost() == "" && sc.GetEmailFrom() == "" {
		return fmt.Errorf("email notififications require that both SMTP Host and EmailFrom be set")
	}
	if sc.GetDefaultRunEvery() <= 0 {
		return fmt.Errorf("default run every must be greater than 0, is %v", sc.GetDefaultRunEvery())
	}
	return nil
}

type RuleConfProvider interface {
	RuleConfWriter // Wrong place for now
	GetUnknownTemplate() *Template
	GetTemplate(string) *Template

	GetAlerts() map[string]*Alert
	GetAlert(string) *Alert
	SetAlert(string, string) (string, error)
	DeleteAlert(alertName string) error

	GetNotifications() map[string]*Notification
	GetNotification(string) *Notification

	GetLookup(string) *Lookup

	AlertSquelched(*Alert) func(opentsdb.TagSet) bool
	Squelched(*Alert, opentsdb.TagSet) bool
	Expand(string, map[string]string, bool) string
	GetFuncs(EnabledBackends) map[string]parse.Func
}

type RuleConfWriter interface {
	BulkEdit(BulkEditRequest) error
	GetRawText() string
	GetHash() string
	SaveRawText(rawConf, diff, user, message string, args ...string) error
	RawDiff(rawConf string) (string, error)
	SetReload(reload func() error)
	SetSaveHook(SaveHook)
}

// Squelch is a map of tag keys to regexes that are applied to tag values. Squelches
// are used to filter results from query responses
type Squelch map[string]*regexp.Regexp

// Squelches is a collection of Squelch
type Squelches []Squelch

// Add adds a sqluech baed on the tags in the first argument. The value of the tag
// is a regular expression. Tags are passed as a string in the format of
func (s *Squelches) Add(v string) error {
	tags, err := opentsdb.ParseTags(v)
	if tags == nil && err != nil {
		return err
	}
	sq := make(Squelch)
	for k, v := range tags {
		re, err := regexp.Compile(v)
		if err != nil {
			return err
		}
		sq[k] = re
	}
	*s = append(*s, sq)
	return nil
}

func (s *Squelches) Squelched(tags opentsdb.TagSet) bool {
	for _, squelch := range *s {
		if squelch.Squelched(tags) {
			return true
		}
	}
	return false
}

func (s Squelch) Squelched(tags opentsdb.TagSet) bool {
	if len(s) == 0 {
		return false
	}
	for k, v := range s {
		tagv, ok := tags[k]
		if !ok || !v.MatchString(tagv) {
			return false
		}
	}
	return true
}

type Template struct {
	Text string
	Vars
	Name    string
	Body    *htemplate.Template `json:"-"`
	Subject *ttemplate.Template `json:"-"`

	RawBody, RawSubject string
	*Locator            `json:"-"`
}

type Notification struct {
	Text string
	Vars
	Name         string
	Email        []*mail.Address
	Post, Get    *url.URL
	Body         *ttemplate.Template
	Print        bool
	Next         *Notification
	Timeout      time.Duration
	ContentType  string
	RunOnActions bool
	UseBody      bool

	NextName        string `json:"-"`
	RawEmail        string `json:"-"`
	RawPost, RawGet string `json:"-"`
	RawBody         string `json:"-"`

	*Locator `json:"-"`
}

type Vars map[string]string

type Notifications struct {
	Notifications map[string]*Notification `json:"-"`
	// Table key -> table
	Lookups map[string]*Lookup
}

// Get returns the set of notifications based on given tags.
func (ns *Notifications) Get(c RuleConfProvider, tags opentsdb.TagSet) map[string]*Notification {
	nots := make(map[string]*Notification)
	for name, n := range ns.Notifications {
		nots[name] = n
	}
	for key, lookup := range ns.Lookups {
		l := lookup.ToExpr()
		val, ok := l.Get(key, tags)
		if !ok {
			continue
		}
		ns := make(map[string]*Notification)
		for _, s := range strings.Split(val, ",") {
			s = strings.TrimSpace(s)
			n := c.GetNotification(s)
			if n == nil {
				continue // TODO error here?
			}
			ns[s] = n
		}
		for name, n := range ns {
			nots[name] = n
		}
	}
	return nots
}

// GetNotificationChains returns the warn or crit notification chains for a configured
// alert. Each chain is a list of notification names. If a notification name
// as already been seen in the chain it ends the list with the notification
// name with a of "..." which indicates that the chain will loop.
func GetNotificationChains(c RuleConfProvider, n map[string]*Notification) [][]string {
	chains := [][]string{}
	for _, root := range n {
		chain := []string{}
		seen := make(map[string]bool)
		var walkChain func(next *Notification)
		walkChain = func(next *Notification) {
			if next == nil {
				chains = append(chains, chain)
				return
			}
			if seen[next.Name] {
				chain = append(chain, fmt.Sprintf("...%v", next.Name))
				chains = append(chains, chain)
				return
			}
			chain = append(chain, next.Name)
			seen[next.Name] = true
			walkChain(next.Next)
		}
		walkChain(root)
	}
	return chains
}

type Lookup struct {
	Text     string
	Name     string
	Tags     []string
	Entries  []*Entry
	*Locator `json:"-"`
}

func (lookup *Lookup) ToExpr() *ExprLookup {
	l := ExprLookup{
		Tags: lookup.Tags,
	}
	for _, entry := range lookup.Entries {
		l.Entries = append(l.Entries, entry.ExprEntry)
	}
	return &l
}

type Entry struct {
	*ExprEntry
	Def  string
	Name string
}

type Macro struct {
	Text     string
	Pairs    interface{} // this is BAD TODO
	Name     string
	*Locator `json:"-"`
}

type Alert struct {
	Text string
	Vars
	*Template        `json:"-"`
	Name             string
	Crit             *expr.Expr `json:",omitempty"`
	Warn             *expr.Expr `json:",omitempty"`
	Depends          *expr.Expr `json:",omitempty"`
	Squelch          Squelches  `json:"-"`
	CritNotification *Notifications
	WarnNotification *Notifications
	Unknown          time.Duration
	MaxLogFrequency  time.Duration
	IgnoreUnknown    bool
	UnknownsNormal   bool
	UnjoinedOK       bool `json:",omitempty"`
	Log              bool
	RunEvery         int
	ReturnType       models.FuncType

	TemplateName string   `json:"-"`
	RawSquelch   []string `json:"-"`

	*Locator `json:"-"`
}

type LocationType int

const (
	TypeNative LocationType = iota
)

type NativeLocator []int

type Locator struct {
	LocatorType LocationType
	Location    interface{}
}

type BulkEditRequest []EditRequest

type EditRequest struct {
	Name   string
	Type   string
	Text   string
	Delete bool
}

type SaveHook func(files, user, message string, args ...string) error

// MakeSaveCommandHook takes a command name and will run it on save
// passing files, user, message, args... as arguments to the command
// the first arg of args is expected to be the file name. In the future
// if we have multifile storage, it could be either a director
func MakeSaveCommandHook(cmdName string) (f SaveHook, err error) {
	_, err = exec.LookPath(cmdName)
	if err != nil {
		return f, fmt.Errorf("command %v not found, failed to create save hook: %v", cmdName, err)
	}
	f = func(files, user, message string, args ...string) error {
		cArgs := []string{files, user, message}
		cArgs = append(cArgs, args...)
		slog.Infof("executing command hook %v\n", cmdName)
		c := exec.Command(cmdName, cArgs...)
		var cOut bytes.Buffer
		var cErr bytes.Buffer
		c.Stdout = &cOut
		c.Stderr = &cErr
		err := c.Start()
		if err != nil {
			return err
		}
		err = c.Wait()
		if err != nil {
			slog.Warning(cErr.String())
			return err
		}
		slog.Infoln(cOut.String())
		return nil
	}
	return
}

func GenHash(s string) string {
	h := fnv.New32a()
	h.Write([]byte(s))
	return fmt.Sprintf("%v", h.Sum32())
}
