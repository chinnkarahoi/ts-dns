package inbound

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/janeczku/go-ipset/ipset"
	"github.com/miekg/dns"
	"github.com/wolf-joe/ts-dns/cache"
	"github.com/wolf-joe/ts-dns/core/common"
	"github.com/wolf-joe/ts-dns/core/context"
	"github.com/wolf-joe/ts-dns/hosts"
	"github.com/wolf-joe/ts-dns/matcher"
	"github.com/wolf-joe/ts-dns/outbound"
)

// Group 各域名组相关配置
type Group struct {
	Callers     []outbound.Caller
	Matcher     *matcher.ABPlus
	IPSet       *ipset.IPSet
	Concurrent  bool
	FastestV4   bool
	TCPPingPort int
	ECS         *dns.EDNS0_SUBNET
	NoCookie    bool
	TestIPv6    []string `toml:"test_ipv6"`
	DisableIPv6 bool
	Name        string
}

// CallDNS 向组内的dns服务器转发请求，可能返回nil
func (group *Group) callDNS(ctx *context.Context, request *dns.Msg) *dns.Msg {
	if len(group.Callers) == 0 || request == nil {
		return nil
	}
	request = request.Copy()
	common.SetDefaultECS(request, group.ECS)
	if group.NoCookie {
		common.RemoveEDNSCookie(request)
	}
	// 并发用的channel
	ch := make(chan *dns.Msg, len(group.Callers))
	// 包裹Caller.Call，方便实现并发
	call := func(caller outbound.Caller, request *dns.Msg) *dns.Msg {
		r, err := caller.Call(request)
		if err != nil {
			log.WithFields(ctx.Fields()).Debugf("query dns error: %v", err)
		}
		ch <- r
		return r
	}
	// 遍历DNS服务器
	for _, caller := range group.Callers {
		log.WithFields(ctx.Fields()).Debugf("forward question %v to %v", request.Question, caller)
		if group.Concurrent || group.FastestV4 {
			go call(caller, request)
		} else if r := call(caller, request); r != nil {
			return r
		}
	}
	// 并发情况下依次提取channel中的返回值
	if group.Concurrent && !group.FastestV4 {
		for i := 0; i < len(group.Callers); i++ {
			if r := <-ch; r != nil {
				return r
			}
		}
	} else if group.FastestV4 { // 选择ping值最低的IPv4地址作为返回值
		return fastestA(ch, len(group.Callers), group.TCPPingPort)
	}
	log.WithFields(ctx.LogFields()).Error("no result found")
	return nil
}

func (group *Group) CallDNS(ctx *context.Context, request *dns.Msg) *dns.Msg {
	records := group.callDNS(ctx, request)
	if group.DisableIPv6 && records != nil {
		for i, record := range records.Answer {
			if _, ok := record.(*dns.AAAA); ok {
				records.Answer[i] = new(dns.AAAA)
			}
		}
	}
	return records
}

// AddIPSet 将dns响应中所有的ipv4地址加入group指定的ipset
func (group *Group) AddIPSet(ctx *context.Context, r *dns.Msg) {
	if group.IPSet == nil || r == nil {
		return
	}
	for _, a := range common.ExtractA(r) {
		if err := group.IPSet.Add(a.A.String(), group.IPSet.Timeout); err != nil {
			log.WithFields(ctx.Fields()).Errorf("add ipset error: %v", err)
		}
	}
	return
}

func testHttpConn(ip string, host string) error {
	url := fmt.Sprintf("http://[%s]", ip)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{
		Timeout: 5 * time.Second,
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	log.Debugf("%s %s %v", ip, host, resp.StatusCode)
	return nil
}

func (group *Group) PollIPv6() {
	if len(group.TestIPv6) == 0 {
		return
	}
	count := 0
	cache := make(map[string]*dns.Msg)
	sleepTime := time.Second * 0
	for {
		disableIPv6 := true
		oldDisableIPv6 := group.DisableIPv6
		for _, domain := range group.TestIPv6 {
			msg := new(dns.Msg)
			msg.SetQuestion(domain+".", dns.TypeAAAA)
			var records *dns.Msg
			if r, ok := cache[domain]; ok && count != 0 {
				records = r
			} else {
				records = group.callDNS(context.NewEmptyContext(0), msg)
			}
			if records == nil {
				continue
			}
			for _, record := range records.Answer {
				if ans, ok := record.(*dns.AAAA); ok {
					cache[domain] = records
					if err := testHttpConn(ans.AAAA.String(), domain); err == nil {
						disableIPv6 = false
						break
					} else {
						log.Debugln(err)
					}
					log.Debugf("%s %s", domain, ans.AAAA.String())
				}
			}
			if !disableIPv6 {
				break
			}
		}
		if disableIPv6 != oldDisableIPv6 {
			group.DisableIPv6 = disableIPv6
			if disableIPv6 {
				log.Infof("%s group IPv6 policy: disable", group.Name)
			} else {
				log.Infof("%s group IPv6 policy: enable", group.Name)
			}
		}
		count = (count + 1) % 10
		if sleepTime <= 30*time.Second {
			sleepTime += time.Second * 5
		}
		time.Sleep(sleepTime)
	}
}

// DNSCache DNS响应缓存器
type reqCond struct {
	cond  *sync.Cond
	ready bool
}

func newReqCond() *reqCond {
	return &reqCond{
		cond:  sync.NewCond(new(sync.Mutex)),
		ready: true,
	}
}

type CondMap map[string]*reqCond

var mu = new(sync.RWMutex)

func (c CondMap) getCacheKey(request *dns.Msg) string {
	question := request.Question[0]
	key := question.Name + strconv.FormatInt(int64(question.Qtype), 10)
	if subnet := common.FormatECS(request); subnet != "" {
		key += "." + subnet
	}
	key = strings.ToLower(key)
	return key
}

func (c CondMap) get(request *dns.Msg) *reqCond {
	key := c.getCacheKey(request)
	mu.RLock()
	cond, ok := c[key]
	mu.RUnlock()
	if ok {
		return cond
	}
	cond = newReqCond()
	mu.Lock()
	c[key] = cond
	mu.Unlock()
	return cond
}

var condMap = make(CondMap)

// Handler 存储主要配置的dns请求处理器，程序核心
type Handler struct {
	Mux           *sync.RWMutex
	Listen        string
	Network       string
	DisableIPv6   bool
	Cache         *cache.DNSCache
	GFWMatcher    *matcher.ABPlus
	CNIP          *cache.RamSet
	CNIPv6        *cache.RamSet
	HostsReaders  []hosts.Reader
	Groups        map[string]*Group
	QueryLogger   *log.Logger
	DisableQTypes map[string]bool
}

// HitHosts 如dns请求匹配hosts，则生成对应dns记录并返回。否则返回nil
func (handler *Handler) HitHosts(ctx *context.Context, request *dns.Msg) *dns.Msg {
	question := request.Question[0]
	if question.Qtype == dns.TypeA || question.Qtype == dns.TypeAAAA {
		ipv6 := question.Qtype == dns.TypeAAAA
		for _, reader := range handler.HostsReaders {
			record, hostname := "", question.Name
			if record = reader.Record(hostname, ipv6); record == "" {
				// 去掉末尾的根域名再找一次
				record = reader.Record(hostname[:len(hostname)-1], ipv6)
			}
			if record != "" {
				if ret, err := dns.NewRR(record); err != nil {
					log.WithFields(ctx.Fields()).Errorf("make DNS.RR error: %v", err)
				} else {
					r := new(dns.Msg)
					r.Answer = append(r.Answer, ret)
					return r
				}
			}
		}
	}
	return nil
}

// LogQuery 记录请求日志
func (handler *Handler) LogQuery(fields log.Fields, msg, group string) {
	entry := handler.QueryLogger.WithFields(fields)
	if group != "" {
		entry = entry.WithField("GROUP", group)
	}
	entry.Info(msg)
}

// ServeDNS 处理dns请求，程序核心函数
func (handler *Handler) ServeDNS(resp dns.ResponseWriter, request *dns.Msg) {
	handler.Mux.RLock() // 申请读锁，持续整个请求
	ctx := context.NewContext(resp, request)
	var r *dns.Msg
	var group *Group
	defer func() {
		if r == nil {
			r = &dns.Msg{}
		}
		r.SetReply(request) // 写入响应
		log.WithFields(ctx.Fields()).Debugf("response: %q", r.Answer)
		_ = resp.WriteMsg(r)
		if group != nil {
			group.AddIPSet(ctx, r) // 写入IPSet
		}
		handler.Mux.RUnlock() // 读锁解除
		_ = resp.Close()      // 结束连接
	}()

	question := request.Question[0]
	log.WithFields(ctx.Fields()).
		Debugf("question: %q, extract: %q", request.Question, request.Extra)
	if handler.DisableIPv6 && question.Qtype == dns.TypeAAAA {
		r = &dns.Msg{}
		return // 禁用IPv6时直接返回
	}
	if qType := dns.TypeToString[question.Qtype]; handler.DisableQTypes[qType] {
		r = &dns.Msg{}
		return // 禁用指定查询类型
	}
	// 检测是否命中hosts
	if r = handler.HitHosts(ctx, request); r != nil {
		handler.LogQuery(ctx.LogFields(), "hit hosts", "")
		return
	}

	reqCond := condMap.get(request)
	reqCond.cond.L.Lock()
	for !reqCond.ready {
		reqCond.cond.Wait()
	}
	reqCond.cond.L.Unlock()

	if r = handler.Cache.Get(request); r != nil {
		handler.LogQuery(ctx.LogFields(), "hit cache", "")
		return
	}

	reqCond.cond.L.Lock()
	for !reqCond.ready {
		reqCond.cond.Wait()
	}
	reqCond.ready = false
	defer func() {
		reqCond.ready = true
		reqCond.cond.Signal()
		reqCond.cond.L.Unlock()
	}()

	if r = handler.Cache.Get(request); r != nil {
		handler.LogQuery(ctx.LogFields(), "hit cache", "")
		return
	}

	// 判断域名是否匹配指定规则
	var name string
	if match, ok := handler.Groups["drop"].Matcher.Match(question.Name); ok && match {
		return
	}
	for name, group = range handler.Groups {
		if match, ok := group.Matcher.Match(question.Name); ok && match {
			handler.LogQuery(ctx.LogFields(), "match by rules", name)
			r = group.CallDNS(ctx, request)
			// 设置dns缓存
			if name == "dirty" && r == nil {
				group := handler.Groups["clean"]
				r = group.CallDNS(ctx, request)
			} else {
				handler.Cache.Set(request, r)
			}
			return
		}
	}
	// 先用clean组dns解析
	usingCache := true
	group = handler.Groups["clean"] // 设置group变量以在defer里添加ipset
	r = group.CallDNS(ctx, request)
	if allInRange(r, handler.CNIP, handler.CNIPv6) {
		// 出现cn ip，流程结束
		if len(common.ExtractA(r))+len(common.ExtractAAAA(r)) == 0 {
			handler.LogQuery(ctx.LogFields(), "no ip found", "none")
		} else {
			handler.LogQuery(ctx.LogFields(), "match cnip", "clean")
		}
	} else {
		// 非cn ip，用dirty组dns再次解析
		group = handler.Groups["dirty"] // 设置group变量以在defer里添加ipset
		rr := group.CallDNS(ctx, request)
		if rr != nil {
			handler.LogQuery(ctx.LogFields(), "not match cnip", "dirty")
			r = rr
		} else {
			handler.LogQuery(ctx.LogFields(), "using clean", "dirty")
			usingCache = false
		}
	}
	// 设置dns缓存
	if usingCache {
		handler.Cache.Set(request, r)
	}
}

// ResolveDoH 为DoHCaller解析域名，只需要调用一次。考虑到回环解析，建议在ServerDNS开始后异步调用
func (handler *Handler) ResolveDoH() {
	resolveDoH := func(caller *outbound.DoHCaller) {
		domain, ip := caller.Host, ""
		// 判断是否有对应Hosts记录
		for _, reader := range handler.HostsReaders {
			if ip = reader.IP(domain, false); ip == "" {
				ip = reader.IP(domain+".", false)
			}
			if ip != "" {
				caller.Servers = append(caller.Servers, ip)
			}
		}
		// 未找到对应hosts记录则使用DoHCaller的Resolve
		if len(caller.Servers) <= 0 {
			if err := caller.Resolve(); err != nil {
				log.Errorf("resolve doh host error: %v", err)
				return
			}
		}
		log.Infof("resolve doh (%s): %v", caller.Host, caller.Servers)
	}
	// 遍历所有DoHCaller解析host
	for _, group := range handler.Groups {
		for _, caller := range group.Callers {
			switch v := caller.(type) {
			case *outbound.DoHCaller:
				resolveDoH(v)
			default:
				continue
			}
		}
	}
}

// Refresh 刷新配置，复制target中除Mux、Listen之外的值
func (handler *Handler) Refresh(target *Handler) {
	handler.Mux.Lock()
	defer handler.Mux.Unlock()

	if target.Cache != nil {
		handler.Cache = target.Cache
	}
	if target.GFWMatcher != nil {
		handler.GFWMatcher = target.GFWMatcher
	}
	if target.CNIP != nil {
		handler.CNIP = target.CNIP
	}
	if target.HostsReaders != nil {
		handler.HostsReaders = target.HostsReaders
	}
	if target.Groups != nil {
		handler.Groups = target.Groups
	}
	if target.QueryLogger != nil {
		handler.QueryLogger = target.QueryLogger
	}
	handler.DisableIPv6 = target.DisableIPv6
}

// IsValid 判断Handler是否符合运行条件
func (handler *Handler) IsValid() bool {
	if handler.Groups == nil {
		return false
	}
	clean, dirty := handler.Groups["clean"], handler.Groups["dirty"]
	if clean == nil || len(clean.Callers) <= 0 || dirty == nil || len(dirty.Callers) <= 0 {
		log.Errorf("dns of clean/dirty group cannot be empty")
		return false
	}
	return true
}
