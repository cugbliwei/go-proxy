package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"sync"
	"time"

	"github.com/go-redis/redis"
)

var proxy = make(map[string]string, 1)
var proxies = []string{}
var lock = &sync.Mutex{}
var hosts = []string{"10.170.10.180:26379", "10.170.11.178:26379", "10.170.12.178:26379"}
var success = "proxies#host#success"
var redisClient = redis.NewFailoverClient(&redis.FailoverOptions{
	MasterName:    "ux_crawl",
	SentinelAddrs: hosts,
})

func tickerProxy() {
	tc := time.NewTicker(time.Minute)
	for _ = range tc.C {
		lock.Lock()
		initProxy()
		lock.Unlock()
	}
}

func initProxy() {
	defer func() {
		if err := recover(); err != nil {
			log.Println("init proxy error: ", err)
			debug.PrintStack()
		}
	}()

	for i := 0; i <= 5; i++ {
		proxy = getProxy()
		if len(proxy) == 0 {
			continue
		} else {
			break
		}
	}

	proxies = []string{}
	for k, _ := range proxy {
		proxies = append(proxies, k)
	}
	log.Println("proxy len: ", len(proxies))
}

func getProxy() map[string]string {
	var target []map[string]interface{}
	res, err := redisClient.HKeys(success).Result()
	if err != nil {
		log.Println("HKeys error: ", err)
		return make(map[string]string, 1)
	}

	for _, ret := range res {
		tg, err := redisClient.HGet(success, ret).Result()
		if err != nil {
			//log.Println("HGet error: ", err, ret)
			continue
		}

		ips := make(map[string]interface{}, 1)
		if err := json.Unmarshal([]byte(tg), &ips); err != nil {
			log.Println("json Unmarshal error: ", err)
			continue
		}

		ips["host"] = ret
		target = append(target, ips)
	}

	result := getConditionProxy(target, 5, 3.0)
	if len(result) < 50 {
		ret := getConditionProxy(target, 3, 6.0)
		result = mergeMap(result, ret)
	}
	if len(result) < 15 {
		ret := getConditionProxy(target, 2, 10.0)
		result = mergeMap(result, ret)
	}
	return result
}

func getConditionProxy(target []map[string]interface{}, times float64, speed float64) map[string]string {
	result := make(map[string]string, 1)
	for _, ips := range target {
		ts, ok := ips["times"].(float64)
		sp, ok1 := ips["avg_speed"].(float64)
		host, ok2 := ips["host"].(string)
		if ok && ok1 && ok2 {
			if ts >= times && sp <= speed {
				result[host] = "1"
			}
		}
	}
	return result
}

func mergeMap(result map[string]string, ret map[string]string) map[string]string {
	for k, v := range ret {
		result[k] = v
	}
	return result
}

func httpclient(proxy string) *http.Client {
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return nil
		},
		Timeout: time.Second * time.Duration(30),
	}

	transport := &http.Transport{
		DisableKeepAlives:     true,
		ResponseHeaderTimeout: time.Second * time.Duration(30),
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
			MaxVersion:         tls.VersionTLS12,
			MinVersion:         tls.VersionTLS10,
			CipherSuites: []uint16{
				tls.TLS_RSA_WITH_RC4_128_SHA,
				tls.TLS_RSA_WITH_3DES_EDE_CBC_SHA,
				tls.TLS_RSA_WITH_AES_128_CBC_SHA,
				tls.TLS_RSA_WITH_AES_256_CBC_SHA,
				tls.TLS_ECDHE_ECDSA_WITH_RC4_128_SHA,
				tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
				tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
				tls.TLS_ECDHE_RSA_WITH_RC4_128_SHA,
				tls.TLS_ECDHE_RSA_WITH_3DES_EDE_CBC_SHA,
				tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
				tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
				tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			},
		},
	}

	if len(proxy) == 0 {
		client.Transport = transport
		return client
	}

	transport.Dial = func(netw, addr string) (net.Conn, error) {
		htimeout := time.Second * time.Duration(30)
		deadline := time.Now().Add(htimeout)
		c, err := net.DialTimeout(netw, addr, htimeout)
		if err != nil {
			return nil, err
		}
		c.SetDeadline(deadline)
		return c, nil
	}
	proxyUrl, err := url.Parse(proxy)
	if err == nil {
		transport.Proxy = http.ProxyURL(proxyUrl)
	}
	client.Transport = transport
	return client
}

type Forward struct{}

func NewForward() *Forward {
	return &Forward{}
}
func (p *Forward) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log.Println("start............................................................")
	defer func() {
		if err := recover(); err != nil {
			log.Println("http proxy request error: ", err)
			debug.PrintStack()
		}
	}()

	//server, err := net.Dial("tcp", r.URL.Host)

	var (
		resp *http.Response
		data []byte
		err  error
	)
	r.RequestURI = ""
	r.ParseForm()

	proxy := ""
	if len(proxies) > 1 {
		rd := rand.New(rand.NewSource(time.Now().UnixNano()))
		randint := rd.Intn(len(proxies) - 1)
		lock.Lock()
		proxy = "http://" + proxies[randint]
		lock.Unlock()
	}

	log.Println("use proxy: ", proxy)
	client := httpclient(proxy)
	resp, err = client.Do(r)
	if err != nil {
		http.NotFound(w, r)
		log.Println("client do error: ", err)
		return
	}
	data, err = ioutil.ReadAll(resp.Body)
	if err != nil && err != io.EOF {
		http.NotFound(w, r)
		log.Println("read body error: ", err)
		return
	}
	for i, j := range resp.Header {
		for _, k := range j {
			w.Header().Add(i, k)
			//log.Println("Header:",i,"=",k)
		}
	}
	for _, c := range resp.Cookies() {
		w.Header().Add("Set-Cookie", c.Raw)
		//log.Println("Set-Cookie",c.Raw)
	}
	_, ok := resp.Header["Content-Length"]
	if !ok && resp.ContentLength > 0 {
		w.Header().Add("Content-Length", fmt.Sprint(resp.ContentLength))
		//log.Println("1、Content-Length", resp.ContentLength)
	}

	//log.Printf("resp.StatusCode:%d  len:%d\n", resp.StatusCode, len(data))
	w.WriteHeader(resp.StatusCode)
	w.Write(data)
}

func main() {
	initProxy()
	go tickerProxy()
	log.Println("Start serving on port 7002")
	forward := NewForward()
	http.ListenAndServe(":7002", forward)
}
