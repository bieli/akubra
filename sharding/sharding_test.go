package sharding

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"testing"

	"github.com/allegro/akubra/config"
	"github.com/allegro/akubra/httphandler"
	set "github.com/deckarep/golang-set"
	"github.com/stretchr/testify/assert"
)

func mkDummySrvsWithfun(count int, t *testing.T, handlerfunc func(w http.ResponseWriter, r *http.Request)) []config.YAMLURL {
	urls := make([]config.YAMLURL, 0, count)
	dummySrvs := make([]*httptest.Server, 0, count)
	for i := 0; i < count; i++ {
		handlerfun := http.HandlerFunc(handlerfunc)
		ts := httptest.NewServer(handlerfun)
		dummySrvs = append(dummySrvs, ts)
		urlN, err := url.Parse(ts.URL)
		if err != nil {
			t.Error(err)
		}
		urls = append(urls, config.YAMLURL{urlN})
	}
	return urls
}

func mkDummySrvs(count int, stream []byte, t *testing.T) []config.YAMLURL {
	f := func(w http.ResponseWriter, r *http.Request) {
		_, err := w.Write(stream)
		assert.Nil(t, err)
	}
	return mkDummySrvsWithfun(count, t, f)
}

var defaultClusterConfig = config.ClusterConfig{
	Type:    "replicator",
	Weight:  1,
	Options: map[string]string{},
}

func configure(backends []config.YAMLURL) config.Config {

	timeout := "3s"
	connLimit := int64(10)
	methodsSlice := []string{"PUT", "GET", "DELETE"}

	methodsSet := set.NewThreadUnsafeSet()
	for _, method := range methodsSlice {
		methodsSet.Add(method)
	}

	syncLogger := log.New(os.Stdout, "sync: ", log.Lshortfile)
	accessLogger := log.New(os.Stdout, "accs: ", log.Lshortfile)
	mainLogger := log.New(os.Stdout, "main: ", log.Lshortfile)
	defaultClusterConfig.Backends = backends

	clustersConf := make(map[string]config.ClusterConfig)
	clustersConf["cluster1"] = defaultClusterConfig

	clientCfg := config.ClientConfig{
		Name:        "client1",
		Clusters:    []string{"cluster1"},
		ShardsCount: 20,
	}

	return config.Config{
		YamlConfig: config.YamlConfig{
			ConnLimit:             connLimit,
			ConnectionTimeout:     timeout,
			ConnectionDialTimeout: timeout,
			Client:                clientCfg,
			Clusters:              clustersConf,
			Backends:              backends,
		},
		SyncLogMethodsSet: methodsSet,
		Synclog:           syncLogger,
		Accesslog:         accessLogger,
		Mainlog:           mainLogger,
	}
}

// func TestClusterTypeMap(t *testing.T) {
// 	cluster1Urls := mkDummySrvs(2, []byte("ok"), t)
// 	cluster2Urls := mkDummySrvs(2, []byte("ok"), t)
// 	conf := configure(cluster1Urls)
// 	conf.Clusters["test"] = config.ClusterConfig{
// 		Weight:   2,
// 		Type:     "replicator",
// 		Backends: cluster2Urls,
// 	}
// 	clusterTypMap, err := mapClusterTypes(conf)
// 	if err != nil {
// 		t.Fail()
// 	}
// 	if len(clusterTypMap) != 2 {
// 		t.Fail()
// 	}
// 	if _, ok := clusterTypMap["test"]; !ok {
// 		t.Fail()
// 	}
// }

// func TestSingleCluster(t *testing.T) {
// 	cluster1Urls := mkDummySrvs(2, []byte("ok"), t)
// 	conf := configure(cluster1Urls)
// 	clusterConf := conf.Clusters["default"]
// 	rtCluster := newReplicatorCluster(conf, clusterConf)
// 	clustr, ok := rtCluster.(cluster)
// 	assert.True(t, ok)
// 	assert.Equal(t, clusterConf.Weight, clustr.Weight)
// }

func TestSingleClusterOnRing(t *testing.T) {
	stream := []byte("cluster1")
	cluster1Urls := mkDummySrvs(2, stream, t)
	conf := configure(cluster1Urls)

	httptransp := httphandler.ConfigureHTTPTransport(conf)
	respHandler := httphandler.NewMultipleResponseHandler(conf)
	ringFactory := newRingFactory(conf, httptransp, respHandler)

	clientRing, err := ringFactory.clientRing(conf.Client)
	if err != nil {
		t.Fail()
	}
	req, _ := http.NewRequest("GET", "http://example.com/index/a", nil)
	resp, err := clientRing.RoundTrip(req)
	assert.Nil(t, err)
	respBody := make([]byte, resp.ContentLength)
	_, err = io.ReadFull(resp.Body, respBody)
	assert.Nil(t, err)
	assert.Equal(t, stream, respBody)
}

func TestTwoClustersOnRing(t *testing.T) {
	response1 := []byte("aaa")
	cluster1Urls := mkDummySrvs(2, response1, t)
	response2 := []byte("bbb")
	cluster2Urls := mkDummySrvs(2, response2, t)
	conf := configure(cluster1Urls)
	conf.Clusters["test"] = config.ClusterConfig{
		Weight:   1,
		Type:     "replicator",
		Backends: cluster2Urls,
	}

	conf.Client.Clusters = append(conf.Client.Clusters, "test")

	httptransp := httphandler.ConfigureHTTPTransport(conf)
	respHandler := httphandler.NewMultipleResponseHandler(conf)
	ringFactory := newRingFactory(conf, httptransp, respHandler)

	clientRing, err := ringFactory.clientRing(conf.Client)

	reader := bytes.NewBuffer([]byte{})

	req, _ := http.NewRequest("PUT", "http://example.com/index/a", reader)
	resp, err := clientRing.RoundTrip(req)
	assert.Nil(t, err)

	respBody := make([]byte, 3)
	_, err = io.ReadFull(resp.Body, respBody)
	assert.Nil(t, err, "cannot read response")
	assert.Equal(t, response1, respBody, "response differs")

	req2, _ := http.NewRequest("PUT", "http://example.com/index/aa", reader)
	resp2, err2 := clientRing.RoundTrip(req2)
	assert.Nil(t, err2)

	respBody2 := make([]byte, 3)
	_, err = io.ReadFull(resp2.Body, respBody2)
	assert.Nil(t, err, "cannot read response")
	assert.Equal(t, response2, respBody2, "response differs")
}

func TestBucketOpDetection(t *testing.T) {
	sr := shardsRing{}
	bucketPaths := []string{"/foo", "/bar/"}
	for _, path := range bucketPaths {
		if !sr.isBucketPath(path) {
			t.Fail()
		}
	}
	nonBucketPaths := []string{"/foo/1", "/bar/1/"}
	for _, path := range nonBucketPaths {
		if sr.isBucketPath(path) {
			t.Fail()
		}
	}
}

func TestTwoClustersOnRingBucketOp(t *testing.T) {
	callCount := 0
	m := sync.Mutex{}
	f := func(w http.ResponseWriter, r *http.Request) {
		m.Lock()
		w.WriteHeader(http.StatusBadRequest)
		callCount++
		m.Unlock()
	}

	cluster1Urls := mkDummySrvsWithfun(2, t, f)
	conf := configure(cluster1Urls)
	cluster2Urls := mkDummySrvsWithfun(2, t, f)
	conf.Clusters["test"] = config.ClusterConfig{
		Weight:   1,
		Type:     "replicator",
		Backends: cluster2Urls,
	}

	conf.Client.Clusters = append(conf.Client.Clusters, "test")
	httptransp := httphandler.ConfigureHTTPTransport(conf)
	respHandler := httphandler.NewMultipleResponseHandler(conf)

	ringFactory := newRingFactory(conf, httptransp, respHandler)

	clientRing, err := ringFactory.clientRing(conf.Client)

	assert.Nil(t, err)
	reader := bytes.NewBuffer([]byte{})
	req, _ := http.NewRequest("PUT", "http://example.com/index/", reader)
	_, err2 := clientRing.RoundTrip(req)
	assert.Nil(t, err2)

	assert.Equal(t, 4, callCount, "No all backends called")
}

func TestTwoClustersOnRingBucketSharding(t *testing.T) {
	callCount := 0
	m := sync.Mutex{}
	f := func(w http.ResponseWriter, r *http.Request) {
		m.Lock()
		w.WriteHeader(http.StatusBadRequest)
		callCount++
		m.Unlock()
	}

	cluster1Urls := mkDummySrvsWithfun(2, t, f)
	conf := configure(cluster1Urls)
	cluster2Urls := mkDummySrvsWithfun(2, t, f)
	conf.Clusters["test"] = config.ClusterConfig{
		Weight:   1,
		Type:     "replicator",
		Backends: cluster2Urls,
	}

	conf.Client.Clusters = append(conf.Client.Clusters, "test")
	httptransp := httphandler.ConfigureHTTPTransport(conf)
	respHandler := httphandler.NewMultipleResponseHandler(conf)

	ringFactory := newRingFactory(conf, httptransp, respHandler)

	clientRing, err := ringFactory.clientRing(conf.Client)

	assert.Nil(t, err)
	reader := bytes.NewBuffer([]byte{})
	req, _ := http.NewRequest("PUT", "http://example.com/index/a", reader)
	_, err2 := clientRing.RoundTrip(req)
	assert.Nil(t, err2)

	assert.Equal(t, 2, callCount, "Too many backends called")
}