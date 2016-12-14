package sharding

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/allegro/akubra/config"
	"github.com/allegro/akubra/httphandler"
	"github.com/allegro/akubra/transport"
	"github.com/golang/groupcache/consistenthash"
)

type cluster struct {
	http.RoundTripper
	weight   uint
	backends []config.YAMLURL
	name     string
}

type shardsRing struct {
	ring                    *consistenthash.Map
	shardClusterMap         map[string]cluster
	allClustersRoundTripper http.RoundTripper
	clusterRegressionMap    map[string]cluster
}

func (sr shardsRing) isBucketPath(path string) bool {
	trimmedPath := strings.Trim(path, "/")
	return len(strings.Split(trimmedPath, "/")) == 1
}

func (sr shardsRing) Pick(key string) (http.RoundTripper, error) {
	var shardName string
	if sr.isBucketPath(key) {
		return sr.allClustersRoundTripper, nil
	}
	shardName = sr.ring.Get(key)
	shardCluster, ok := sr.shardClusterMap[shardName]
	if !ok {
		return nil, fmt.Errorf("no cluster for shard %s, cannot handle key %s", shardName, key)
	}

	return shardCluster, nil
}

func (sr shardsRing) regressionCall(cl http.RoundTripper, req *http.Request) (*http.Response, error) {
	resp, err := cl.RoundTrip(req)
	// Do regression call if response status is > 400
	if err != nil || resp.StatusCode > 400 {
		curCluster, ok := cl.(cluster)
		if ok {
			regressionCluster, exists := sr.clusterRegressionMap[curCluster.name]
			if exists {
				return sr.regressionCall(regressionCluster, req)
			}
		}
	}
	return resp, err
}

func (sr shardsRing) RoundTrip(req *http.Request) (*http.Response, error) {

	if req.Method == http.MethodDelete {
		return sr.allClustersRoundTripper.RoundTrip(req)
	}

	cl, err := sr.Pick(req.URL.Path)
	if err != nil {
		return nil, err
	}

	return sr.regressionCall(cl, req)
}

func newMultiBackendCluster(transp http.RoundTripper,
	multiResponseHandler transport.MultipleResponsesHandler,
	clusterConf config.ClusterConfig, name string) cluster {
	backends := make([]*url.URL, len(clusterConf.Backends))

	for i, backend := range clusterConf.Backends {
		backends[i] = backend.URL
	}

	multiTransport := transport.NewMultiTransport(
		transp,
		backends,
		multiResponseHandler)

	return cluster{
		multiTransport,
		clusterConf.Weight,
		clusterConf.Backends,
		name,
	}
}

type ringFactory struct {
	conf                    config.Config
	transport               http.RoundTripper
	multipleResponseHandler transport.MultipleResponsesHandler
	clusters                map[string]cluster
}

func (rf ringFactory) initCluster(name string) (cluster, error) {
	clusterConf, ok := rf.conf.Clusters[name]
	if !ok {
		return cluster{}, fmt.Errorf("no cluster %q in configuration", name)
	}
	return newMultiBackendCluster(rf.transport, rf.multipleResponseHandler, clusterConf, name), nil
}

func (rf ringFactory) getCluster(name string) (cluster, error) {
	s3cluster, ok := rf.clusters[name]
	if ok {
		return s3cluster, nil
	}
	s3cluster, err := rf.initCluster(name)
	if err != nil {
		return s3cluster, err
	}
	rf.clusters[name] = s3cluster
	return s3cluster, nil
}

func (rf ringFactory) mapShards(weightSum uint, clientCfg config.ClientConfig) (map[string]cluster, error) {
	shardClusterMap := make(map[string]cluster, clientCfg.ShardsCount)
	offset := 0
	for _, name := range clientCfg.Clusters {
		clientCluster, err := rf.getCluster(name)
		if err != nil {
			return shardClusterMap, err
		}
		shardsNum := float64(clientCfg.ShardsCount) * float64(clientCluster.weight) / float64(weightSum)
		for i := offset; i < offset+int(shardsNum); i++ {
			shardName := fmt.Sprintf("%s-%d", clientCfg.Name, i)
			shardClusterMap[shardName] = clientCluster
		}
		offset += int(shardsNum)
	}
	return shardClusterMap, nil
}

func (rf ringFactory) uniqBackends(clientCfg config.ClientConfig) ([]*url.URL, error) {
	allBackendsSet := make(map[config.YAMLURL]bool)
	for _, name := range clientCfg.Clusters {
		clientCluster, err := rf.getCluster(name)
		if err != nil {
			return nil, err
		}
		for _, backendURL := range clientCluster.backends {
			allBackendsSet[backendURL] = true
		}
	}
	var uniqBackendsSlice []*url.URL
	for url := range allBackendsSet {
		uniqBackendsSlice = append(uniqBackendsSlice, url.URL)
	}
	return uniqBackendsSlice, nil
}

func (rf ringFactory) clientRing(clientCfg config.ClientConfig) (shardsRing, error) {
	weightSum := uint(0)
	clientClusters := make([]cluster, 0, len(clientCfg.Clusters))
	regressionMap := make(map[string]cluster)
	var previousCluster cluster
	for i, name := range clientCfg.Clusters {
		clientCluster, err := rf.getCluster(name)
		if err != nil {
			return shardsRing{}, err
		}

		weightSum += clientCluster.weight
		clientClusters = append(clientClusters, clientCluster)

		if i > 0 {
			regressionMap[name] = previousCluster
		}
		previousCluster = clientCluster
	}
	shardMap, err := rf.mapShards(weightSum, clientCfg)
	if err != nil {
		return shardsRing{}, err
	}
	cHashMap := consistenthash.New(1, nil)
	for shardID := range shardMap {
		cHashMap.Add(shardID)
	}

	allBackendsSlice, err := rf.uniqBackends(clientCfg)
	if err != nil {
		return shardsRing{}, err
	}
	allBackendsRoundTripper := transport.NewMultiTransport(
		rf.transport,
		allBackendsSlice,
		rf.multipleResponseHandler)

	return shardsRing{cHashMap, shardMap, allBackendsRoundTripper, regressionMap}, nil
}

func newRingFactory(conf config.Config, transport http.RoundTripper, respHandler transport.MultipleResponsesHandler) ringFactory {
	return ringFactory{
		conf:                    conf,
		transport:               transport,
		multipleResponseHandler: respHandler,
		clusters:                make(map[string]cluster),
	}
}

//NewHandler constructs http.Handler
func NewHandler(conf config.Config) http.Handler {
	// clustersMap, _ := mapClusterTypes(conf)
	clustersNames := make([]string, 0, len(conf.Clusters))
	for name := range conf.Clusters {
		clustersNames = append(clustersNames, name)
	}

	conf.Mainlog.Printf("Configured clusters: %s", strings.Join(clustersNames, ", "))

	httptransp := httphandler.ConfigureHTTPTransport(conf)
	respHandler := httphandler.NewMultipleResponseHandler(conf)
	ringFactory := newRingFactory(conf, httptransp, respHandler)
	//TODO: Multiple clients
	ring, err := ringFactory.clientRing(conf.Client)
	if err != nil {
		conf.Mainlog.Fatalln("Setup error:", err.Error())
	}

	conf.Mainlog.Printf("Ring sharded into %d partitions", len(ring.shardClusterMap))

	roundTripper := httphandler.DecorateRoundTripper(conf, ring)
	return httphandler.NewHandlerWithRoundTripper(conf, roundTripper)
}
