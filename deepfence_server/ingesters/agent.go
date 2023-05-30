package ingesters

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/deepfence/ThreatMapper/deepfence_server/pkg/scope/report"
	"github.com/deepfence/golang_deepfence_sdk/utils/directory"
	"github.com/deepfence/golang_deepfence_sdk/utils/log"
	"github.com/deepfence/golang_deepfence_sdk/utils/telemetry"
	"github.com/deepfence/golang_deepfence_sdk/utils/utils"
	"github.com/neo4j/neo4j-go-driver/v4/neo4j"
	"github.com/neo4j/neo4j-go-driver/v4/neo4j/db"
	redis2 "github.com/redis/go-redis/v9"
)

const (
	REDIS_NETWORK_MAP_KEY   = "network_map"
	REDIS_IPPORTPID_MAP_KEY = "ipportpid_map"
	uncompress_workers_num  = 50
	preparer_workers_num    = 5
	default_db_input_size   = 20
	db_batch_size           = 1_000
	resolver_batch_size     = 1_000
	default_ingester_size   = default_db_input_size * db_batch_size
	db_batch_timeout        = time.Second * 10
	resolver_timeout        = time.Second * 10
	max_network_maps_size   = 1024 * 1024 * 1024 // 1 GB per maps
	enqueer_timeout         = time.Second * 30
	agent_base_timeout      = time.Second * 30
	localhost_ip            = "127.0.0.1"
	default_push_back       = 1
)

var (
	breaker       atomic.Bool
	Push_back     atomic.Int32
	ingester_size int
	db_input_size int
)

func init() {
	breaker.Store(false)
	Push_back.Store(default_push_back)
	push := os.Getenv("DF_INGEST_PUSH_BACK")
	if push != "" {
		push_int, err := strconv.Atoi(push)
		if err == nil {
			Push_back.Store(int32(push_int))
		}
	}

	ingester_size = default_ingester_size
	bsize := os.Getenv("DF_INGEST_REPORT_SIZE")
	if bsize != "" {
		ingester_size, _ = strconv.Atoi(bsize)
	}

	db_input_size = default_db_input_size
	dbsize := os.Getenv("DF_INGEST_DB_SIZE")
	if dbsize != "" {
		db_input_size, _ = strconv.Atoi(dbsize)
	}
}

type EndpointResolvers struct {
	network_map  map[string]string
	ipport_ippid map[string]string
}

func (er *EndpointResolvers) clean() {
	for k := range er.network_map {
		delete(er.network_map, k)
	}
	for k := range er.ipport_ippid {
		delete(er.ipport_ippid, k)
	}
}

type EndpointResolversCache struct {
	rdb *redis2.Client
}

func newEndpointResolversCache(ctx context.Context) (EndpointResolversCache, error) {
	rdb, err := directory.RedisClient(ctx)
	return EndpointResolversCache{
		rdb: rdb,
	}, err
}

func (erc *EndpointResolversCache) clean_maps() {
	if v, _ := erc.rdb.MemoryUsage(context.Background(), REDIS_NETWORK_MAP_KEY).Result(); v > max_network_maps_size {
		log.Debug().Msgf("Memory usage for %v reached limit", REDIS_NETWORK_MAP_KEY)
		erc.rdb.HDel(context.Background(), REDIS_NETWORK_MAP_KEY)
	}
	if v, _ := erc.rdb.MemoryUsage(context.Background(), REDIS_IPPORTPID_MAP_KEY).Result(); v > max_network_maps_size {
		log.Debug().Msgf("Memory usage for %v reached limit", REDIS_IPPORTPID_MAP_KEY)
		erc.rdb.HDel(context.Background(), REDIS_IPPORTPID_MAP_KEY)
	}
}

func (erc *EndpointResolversCache) push_maps(er *EndpointResolvers) {
	erc.rdb.HSet(context.Background(), REDIS_NETWORK_MAP_KEY, er.network_map)
	erc.rdb.HSet(context.Background(), REDIS_IPPORTPID_MAP_KEY, er.ipport_ippid)
}

func (erc *EndpointResolversCache) get_host(ip string) (string, bool) {
	res, err := erc.rdb.HGet(context.Background(), REDIS_NETWORK_MAP_KEY, ip).Result()
	return res, err == nil
}

func (erc *EndpointResolversCache) get_ip_pid(ip_port string) (string, bool) {
	res, err := erc.rdb.HGet(context.Background(), REDIS_IPPORTPID_MAP_KEY, ip_port).Result()
	return res, err == nil
}

func (erc *EndpointResolversCache) Close() {
}

type neo4jIngester struct {
	driver           neo4j.Driver
	ingester         chan report.CompressedReport
	resolvers        EndpointResolversCache
	batcher          chan ReportIngestionData
	resolvers_update chan EndpointResolvers
	preparers_input  chan report.Report
	done             chan struct{}
	db_pusher        chan ReportIngestionData
	num_ingested     atomic.Int32
	num_received     atomic.Int32
}

type ReportIngestionData struct {
	Process_batch            []map[string]interface{} `json:"process_batch" required:"true"`
	Host_batch               []map[string]interface{} `json:"host_batch" required:"true"`
	Container_batch          []map[string]interface{} `json:"container_batch" required:"true"`
	Pod_batch                []map[string]interface{} `json:"pod_batch" required:"true"`
	Container_image_batch    []map[string]interface{} `json:"container_image_batch" required:"true"`
	Kubernetes_cluster_batch []map[string]interface{} `json:"kubernetes_cluster_batch" required:"true"`

	Process_edges_batch           []map[string]interface{} `json:"process_edges_batch" required:"true"`
	Container_edges_batch         []map[string]interface{} `json:"container_edges_batch" required:"true"`
	Container_process_edges_batch []map[string]interface{} `json:"container_process_edge_batch" required:"true"`
	Pod_edges_batch               []map[string]interface{} `json:"pod_edges_batch" required:"true"`
	Pod_host_edges_batch          []map[string]interface{} `json:"pod_host_edges_batch" required:"true"`
	Endpoint_edges_batch          []map[string]interface{} `json:"endpoint_edges_batch" required:"true"`
	Container_image_edge_batch    []map[string]interface{} `json:"container_image_edge_batch" required:"true"`
	Kubernetes_cluster_edge_batch []map[string]interface{} `json:"kubernetes_cluster_edge_batch" required:"true"`

	//Endpoint_batch []map[string]string
	//Endpoint_edges []map[string]string

	Hosts     []map[string]interface{} `json:"hosts" required:"true"`
	NumMerged int                      `json:"num_merged" required:"true"`
}

func mergeResolvers(others []EndpointResolvers) EndpointResolvers {
	size_network_map := 0
	size_ipport_ippid := 0
	for i := range others {
		size_network_map += len(others[i].network_map)
		size_ipport_ippid += len(others[i].ipport_ippid)
	}

	res := EndpointResolvers{
		network_map:  make(map[string]string, size_network_map),
		ipport_ippid: make(map[string]string, size_ipport_ippid),
	}

	for i := range others {
		for k, v := range others[i].network_map {
			res.network_map[k] = v
		}
		for k, v := range others[i].ipport_ippid {
			res.ipport_ippid[k] = v
		}
	}
	return res
}

func mergeIngestionData(other []ReportIngestionData) ReportIngestionData {

	size_process_batch := 0
	size_host_batch := 0
	size_container_batch := 0
	size_container_image_batch := 0
	size_pod_batch := 0
	size_kubernetes_cluster_batch := 0
	size_process_edges_batch := 0
	size_container_process_edges_batch := 0
	size_container_edges_batch := 0
	size_pod_edges_batch := 0
	size_endpoint_edges_batch := 0
	size_container_image_edge_batch := 0
	size_kubernetes_cluster_edge_batch := 0
	size_hosts := 0

	for i := range other {
		size_process_batch += len(other[i].Process_batch)
		size_host_batch += len(other[i].Host_batch)
		size_container_batch += len(other[i].Container_batch)
		size_container_image_batch += len(other[i].Container_image_batch)
		size_pod_batch += len(other[i].Pod_batch)
		size_kubernetes_cluster_batch += len(other[i].Kubernetes_cluster_batch)
		size_process_edges_batch += len(other[i].Process_edges_batch)
		size_container_process_edges_batch += len(other[i].Container_process_edges_batch)
		size_container_edges_batch += len(other[i].Container_edges_batch)
		size_pod_edges_batch += len(other[i].Pod_edges_batch)
		size_endpoint_edges_batch += len(other[i].Endpoint_edges_batch)
		size_container_image_edge_batch += len(other[i].Container_image_edge_batch)
		size_kubernetes_cluster_edge_batch += len(other[i].Kubernetes_cluster_edge_batch)
		size_hosts += len(other[i].Host_batch)
	}

	process_batch := make([]map[string]interface{}, 0, size_process_batch)
	host_batch := make([]map[string]interface{}, 0, size_host_batch)
	container_batch := make([]map[string]interface{}, 0, size_container_batch)
	container_image_batch := make([]map[string]interface{}, 0, size_container_image_batch)
	pod_batch := make([]map[string]interface{}, 0, size_pod_batch)
	kubernetes_cluster_batch := make([]map[string]interface{}, 0, size_kubernetes_cluster_batch)
	process_edges_batch := make([]map[string]interface{}, 0, size_process_edges_batch)
	container_process_edges_batch := make([]map[string]interface{}, 0, size_container_process_edges_batch)
	container_edges_batch := make([]map[string]interface{}, 0, size_container_edges_batch)
	pod_edges_batch := make([]map[string]interface{}, 0, size_pod_edges_batch)
	endpoint_edges_batch := make([]map[string]interface{}, 0, size_endpoint_edges_batch)
	container_image_edge_batch := make([]map[string]interface{}, 0, size_container_image_edge_batch)
	kubernetes_cluster_edge_batch := make([]map[string]interface{}, 0, size_kubernetes_cluster_edge_batch)
	hosts := make([]map[string]interface{}, 0, size_hosts)

	for i := range other {
		process_batch = append(process_batch, other[i].Process_batch...)
		host_batch = append(host_batch, other[i].Host_batch...)
		container_batch = append(container_batch, other[i].Container_batch...)
		container_image_batch = append(container_image_batch, other[i].Container_image_batch...)
		pod_batch = append(pod_batch, other[i].Pod_batch...)
		kubernetes_cluster_batch = append(kubernetes_cluster_batch, other[i].Kubernetes_cluster_batch...)
		process_edges_batch = append(process_edges_batch, other[i].Process_edges_batch...)
		container_process_edges_batch = append(container_process_edges_batch, other[i].Container_process_edges_batch...)
		container_edges_batch = append(container_edges_batch, other[i].Container_edges_batch...)
		pod_edges_batch = append(pod_edges_batch, other[i].Pod_edges_batch...)
		endpoint_edges_batch = append(endpoint_edges_batch, other[i].Endpoint_edges_batch...)
		container_image_edge_batch = append(container_image_edge_batch, other[i].Container_image_edge_batch...)
		kubernetes_cluster_edge_batch = append(kubernetes_cluster_edge_batch, other[i].Kubernetes_cluster_edge_batch...)
		//nd.Endpoint_batch = append(nd.Endpoint_batch, other[i].Endpoint_batch...)
		//nd.Endpoint_edges = append(nd.Endpoint_edges, other[i].Endpoint_edges...)
		hosts = append(hosts, other[i].Hosts...)
	}

	return ReportIngestionData{
		Process_batch:                 process_batch,
		Host_batch:                    host_batch,
		Container_batch:               container_batch,
		Container_image_batch:         container_image_batch,
		Pod_batch:                     pod_batch,
		Kubernetes_cluster_batch:      kubernetes_cluster_batch,
		Process_edges_batch:           process_edges_batch,
		Container_process_edges_batch: container_process_edges_batch,
		Container_edges_batch:         container_edges_batch,
		Pod_edges_batch:               pod_edges_batch,
		Endpoint_edges_batch:          endpoint_edges_batch,
		Container_image_edge_batch:    container_image_edge_batch,
		Kubernetes_cluster_edge_batch: kubernetes_cluster_edge_batch,
		Hosts:                         hosts,
		NumMerged:                     len(other),
	}

}

func computeResolvers(rpt *report.Report, buf *bytes.Buffer) EndpointResolvers {
	resolvers := EndpointResolvers{
		network_map:  map[string]string{},
		ipport_ippid: map[string]string{},
	}

	for _, n := range rpt.Host {
		if n.Metadata.InterfaceIps == nil {
			continue
		}
		for _, k := range n.Metadata.InterfaceIps {
			resolvers.network_map[k] = n.Metadata.HostName
		}
	}

	for _, n := range rpt.Endpoint {
		if n.Metadata.HostName == "" {
			continue
		}
		node_ip, node_port := extractIPPortFromEndpointID(n.Metadata.NodeID)
		if node_ip == localhost_ip {
			continue
		}
		resolvers.network_map[node_ip] = n.Metadata.HostName
		buf.Reset()
		buf.WriteString(node_ip)
		buf.WriteByte(';')
		buf.WriteString(strconv.Itoa(n.Metadata.Pid))
		resolvers.ipport_ippid[node_ip+node_port] = buf.String()
	}

	return resolvers
}

func (nc *neo4jIngester) resolversUpdater() {
	batch := [db_batch_size]EndpointResolvers{}
	elements := 0
	send := false
	ticker := time.NewTicker(resolver_timeout)
	defer ticker.Stop()
loop:
	for {
		select {
		case resolver, open := <-nc.resolvers_update:
			if !open {
				break loop
			}
			batch[elements] = resolver
			elements += 1
			send = elements == resolver_batch_size
		case <-ticker.C:
			send = elements > 0
		}

		if send {
			send = false
			span := telemetry.NewSpan(context.Background(), "ingester", "ResolversUpdater")
			final_batch := mergeResolvers(batch[:elements])
			nc.resolvers.clean_maps()
			nc.resolvers.push_maps(&final_batch)
			batch = [db_batch_size]EndpointResolvers{}
			span.End()
			elements = 0
		}
	}
	log.Info().Msgf("resolversUpdater ended")
}

func concatMaps(input map[string][]string) []map[string]interface{} {
	res := make([]map[string]interface{}, 0, len(input))
	for k, e := range input {
		res = append(res, map[string]interface{}{"source": k, "destinations": e})
	}
	return res
}

type Connection struct {
	source      string
	destination string
	left_pid    int
	right_pid   int
}

func connections2maps(connections []Connection) []map[string]interface{} {
	delim := ";;;"
	unique_maps := map[string]map[string][]int{}
	for _, connection := range connections {
		connection_id := connection.source + delim + connection.destination
		v, has := unique_maps[connection_id]
		if !has {
			unique_maps[connection_id] = map[string][]int{
				"left_pids":  {connection.left_pid},
				"right_pids": {connection.right_pid},
			}
		} else {
			unique_maps[connection_id]["left_pids"] = append(v["left_pids"], connection.left_pid)
			unique_maps[connection_id]["right_pids"] = append(v["right_pids"], connection.right_pid)
		}
	}
	res := make([]map[string]interface{}, 0, len(unique_maps))
	for k, v := range unique_maps {
		source_dest := strings.Split(k, delim)
		internal := map[string]interface{}{}
		internal["source"] = source_dest[0]
		internal["destination"] = source_dest[1]
		pids := []map[string]int{}
		left_pids := v["left_pids"]
		right_pids := v["right_pids"]
		for i := range left_pids {
			pids = append(pids, map[string]int{
				"left":  left_pids[i],
				"right": right_pids[i],
			})
		}
		internal["pids"] = pids
		res = append(res, internal)
	}
	return res
}

func NewReportIngestionData() ReportIngestionData {
	return ReportIngestionData{
		Process_batch:                 []map[string]interface{}{},
		Host_batch:                    []map[string]interface{}{},
		Container_batch:               []map[string]interface{}{},
		Pod_batch:                     []map[string]interface{}{},
		Container_image_batch:         []map[string]interface{}{},
		Kubernetes_cluster_batch:      []map[string]interface{}{},
		Process_edges_batch:           []map[string]interface{}{},
		Container_edges_batch:         []map[string]interface{}{},
		Container_process_edges_batch: []map[string]interface{}{},
		Pod_edges_batch:               []map[string]interface{}{},
		Pod_host_edges_batch:          []map[string]interface{}{},
		Endpoint_edges_batch:          []map[string]interface{}{},
		Container_image_edge_batch:    []map[string]interface{}{},
		Kubernetes_cluster_edge_batch: []map[string]interface{}{},
		Hosts:                         []map[string]interface{}{},
		NumMerged:                     1,
	}
}

func prepareNeo4jIngestion(rpt *report.Report, resolvers *EndpointResolversCache, buf *bytes.Buffer) ReportIngestionData {

	res := ReportIngestionData{
		Hosts:                         make([]map[string]interface{}, 0, len(rpt.Host)),
		Process_batch:                 make([]map[string]interface{}, 0, len(rpt.Process)),
		Host_batch:                    make([]map[string]interface{}, 0, len(rpt.Host)),
		Container_batch:               make([]map[string]interface{}, 0, len(rpt.Container)),
		Pod_batch:                     make([]map[string]interface{}, 0, len(rpt.Pod)),
		Container_image_batch:         make([]map[string]interface{}, 0, len(rpt.ContainerImage)),
		Kubernetes_cluster_batch:      make([]map[string]interface{}, 0, len(rpt.KubernetesCluster)),
		Process_edges_batch:           nil,
		Container_edges_batch:         nil,
		Container_process_edges_batch: nil,
		Pod_edges_batch:               nil,
		Pod_host_edges_batch:          nil,
		Endpoint_edges_batch:          nil,
		Container_image_edge_batch:    nil,
		Kubernetes_cluster_edge_batch: nil,
		NumMerged:                     1,
	}

	kubernetes_edges_batch := map[string][]string{}

	for _, n := range rpt.Host {
		res.Hosts = append(res.Hosts, map[string]interface{}{"node_id": n.Metadata.NodeID})
		metadataMap := metadataToMap(n.Metadata)
		if n.Metadata.KubernetesClusterId != "" {
			kubernetes_edges_batch[n.Metadata.KubernetesClusterId] = append(kubernetes_edges_batch[n.Metadata.KubernetesClusterId], n.Metadata.NodeID)
		}
		res.Host_batch = append(res.Host_batch, metadataMap)
	}

	for _, n := range rpt.KubernetesCluster {
		res.Kubernetes_cluster_batch = append(res.Kubernetes_cluster_batch, metadataToMap(n.Metadata))
	}

	processes_to_keep := map[string]struct{}{}
	for _, n := range rpt.Endpoint {
		if n.Metadata.HostName == "" {
			continue
		}
		buf.Reset()
		buf.WriteString(n.Metadata.HostName)
		buf.WriteByte(';')
		buf.WriteString(strconv.Itoa(n.Metadata.Pid))
		host_pid := buf.String()
		processes_to_keep[host_pid] = struct{}{}
	}

	//endpoint_batch := []map[string]string{}
	//endpoint_edges := []map[string]string{}

	connections := []Connection{}
	local_memoization := map[string]struct{}{}
	for _, n := range rpt.Endpoint {
		node_ip, _ := extractIPPortFromEndpointID(n.Metadata.NodeID)
		if node_ip == localhost_ip {
			continue
		}
		if n.Metadata.HostName == "" {
			if val, ok := resolvers.get_host(node_ip); ok {
				n.Metadata.HostName = val
			} else {
				continue
			}
		}
		if n.Metadata.Pid != -1 {
			if n.Adjacency == nil || len(*n.Adjacency) == 0 {
				// Handle inbound from internet
				connections = append(connections, Connection{
					source:      "in-the-internet",
					destination: n.Metadata.HostName,
					left_pid:    0,
					right_pid:   n.Metadata.Pid,
				})
			} else {
				for _, i := range *n.Adjacency {
					if n.Metadata.NodeID != i {
						ip, port := extractIPPortFromEndpointID(i)
						// local memoization is used to skip redis access (91% reduction)
						if _, has := local_memoization[ip]; has {
							continue
						}
						if host, ok := resolvers.get_host(ip); ok {
							if n.Metadata.HostName == host {
								local_memoization[ip] = struct{}{}
								continue
							}
							right_ippid, ok := resolvers.get_ip_pid(ip + port)
							if ok {
								rightpid := extractPidFromNodeID(right_ippid)
								connections = append(connections, Connection{
									source:      n.Metadata.HostName,
									destination: host,
									left_pid:    n.Metadata.Pid,
									right_pid:   rightpid,
								})
							}
						} else {
							connections = append(connections, Connection{
								source:      n.Metadata.HostName,
								destination: "out-the-internet",
								left_pid:    n.Metadata.Pid,
								right_pid:   0,
							})
						}
					}
				}
			}
		}
	}
	res.Endpoint_edges_batch = connections2maps(connections)

	process_edges_batch := map[string][]string{}
	container_process_edges_batch := map[string][]string{}
	for _, n := range rpt.Process {
		if _, ok := processes_to_keep[n.Metadata.NodeID]; !ok {
			continue
		}
		res.Process_batch = append(res.Process_batch, metadataToMap(n.Metadata))
		process_edges_batch[n.Metadata.HostName] = append(process_edges_batch[n.Metadata.HostName], n.Metadata.NodeID)
		if len(n.Parents.Container) != 0 {
			container_process_edges_batch[n.Parents.Container] = append(container_process_edges_batch[n.Parents.Container], n.Metadata.NodeID)
		}
	}

	container_edges_batch := map[string][]string{}
	for _, n := range rpt.Container {
		if n.Metadata.HostName == "" {
			continue
		}
		res.Container_batch = append(res.Container_batch, metadataToMap(n.Metadata))
		container_edges_batch[n.Metadata.HostName] = append(container_edges_batch[n.Metadata.HostName], n.Metadata.NodeID)
	}

	container_image_edges_batch := map[string][]string{}
	for _, n := range rpt.ContainerImage {
		if n.Metadata.HostName == "" {
			continue
		}
		res.Container_image_batch = append(res.Container_image_batch, metadataToMap(n.Metadata))
		container_image_edges_batch[n.Metadata.HostName] = append(container_image_edges_batch[n.Metadata.HostName], n.Metadata.NodeID)
	}

	// Note: Pods are provided alone with an extra report
	// Therefore, it cannot rely on any previously computed data
	pod_edges_batch := map[string][]string{}
	pod_host_edges_batch := map[string][]string{}
	for _, n := range rpt.Pod {
		if n.Metadata.KubernetesClusterId == "" {
			continue
		}
		if n.Metadata.HostName == "" {
			if val, ok := resolvers.get_host(n.Metadata.KubernetesIP); ok {
				n.Metadata.HostName = val
			}
		}
		res.Pod_batch = append(res.Pod_batch, metadataToMap(n.Metadata))
		pod_edges_batch[n.Metadata.KubernetesClusterId] = append(pod_edges_batch[n.Metadata.KubernetesClusterId], n.Metadata.NodeID)
		pod_host_edges_batch[n.Metadata.HostName] = append(pod_host_edges_batch[n.Metadata.HostName], n.Metadata.NodeID)
	}

	res.Process_edges_batch = concatMaps(process_edges_batch)
	res.Container_edges_batch = concatMaps(container_edges_batch)
	res.Container_process_edges_batch = concatMaps(container_process_edges_batch)
	res.Pod_edges_batch = concatMaps(pod_edges_batch)
	res.Pod_host_edges_batch = concatMaps(pod_host_edges_batch)
	res.Container_image_edge_batch = concatMaps(container_image_edges_batch)
	res.Kubernetes_cluster_edge_batch = concatMaps(kubernetes_edges_batch)

	return res
}

func (nc *neo4jIngester) Close() {
	nc.resolvers.Close()
	close(nc.ingester)
	close(nc.batcher)
	close(nc.resolvers_update)
	close(nc.preparers_input)
	close(nc.done)
}

func (nc *neo4jIngester) Ingest(ctx context.Context, crpt report.CompressedReport) error {
	select {
	case nc.ingester <- crpt:
		nc.num_received.Add(1)
	default:
		breaker.Store(true)
		crpt.Cleanup()
		return fmt.Errorf("enqueuer channel full")
	}
	return nil
}

func (nc *neo4jIngester) IsReady() bool {
	return !breaker.Load()
}

func (nc *neo4jIngester) PushToDB(batches ReportIngestionData, session neo4j.Session) error {
	tx, err := session.BeginTransaction()
	if err != nil {
		return err
	}
	defer tx.Close()

	if _, err := tx.Run(`
		UNWIND $batch as row
		MERGE (cp:CloudProvider{node_id:row.cloud_provider})
		MERGE (cr:CloudRegion{node_id:row.cloud_region})
		MERGE (cp) -[:HOSTS]-> (cr)
		MERGE (n:Node{node_id:row.node_id})
		MERGE (cr) -[:HOSTS]-> (n)
		SET n+= row, n.updated_at = TIMESTAMP(), n.active = true, cp.active = true, cp.pseudo = false, cr.active = true`,
		map[string]interface{}{"batch": batches.Host_batch}); err != nil {
		return err
	}

	if _, err := tx.Run(`
		UNWIND $batch as row
		MERGE (n:Container{node_id:row.node_id})
		SET n+= row, n.updated_at = TIMESTAMP(), n.active = row.docker_container_state <> "deleted"`,
		map[string]interface{}{"batch": batches.Container_batch}); err != nil {
		return err
	}

	if _, err := tx.Run(`
		UNWIND $batch as row
		MERGE (n:ContainerImage{node_id:row.node_id})
		MERGE (s:ImageStub{node_id: row.docker_image_name})
		MERGE (n) -[:IS]-> (s)
		SET n+= row, n.updated_at = TIMESTAMP(), n.active = true`,
		map[string]interface{}{"batch": batches.Container_image_batch}); err != nil {
		return err
	}

	if _, err := tx.Run(`
		UNWIND $batch as row
		MERGE (n:Pod{node_id:row.node_id})
		SET n+= row, n.updated_at = TIMESTAMP(), n.active = true`,
		map[string]interface{}{"batch": batches.Pod_batch}); err != nil {
		return err
	}

	if _, err := tx.Run(`
		UNWIND $batch as row
		MERGE (n:KubernetesCluster{node_id:row.node_id})
		MERGE (cp:CloudProvider{node_id:row.cloud_provider})
		MERGE (cp) -[:HOSTS]-> (n)
		SET n+= row, n.updated_at = TIMESTAMP(), n.active = true, n.node_type = 'cluster', cp.active = true, cp.pseudo = false`,
		map[string]interface{}{"batch": batches.Kubernetes_cluster_batch}); err != nil {
		return err
	}

	if _, err = tx.Run(`
		UNWIND $batch as row
		MERGE (n:Process{node_id:row.node_id})
		SET n+= row, n.updated_at = TIMESTAMP()`,
		map[string]interface{}{"batch": batches.Process_batch}); err != nil {
		return err
	}

	//if _, err = tx.Run("UNWIND $batch as row MERGE (n:TEndpoint{node_id:row.node_id}) SET n+= row", map[string]interface{}{"batch": batches.Endpoint_batch}); err != nil {
	//return err
	//}

	if _, err = tx.Run(`
		UNWIND $batch as row
		MATCH (n:Container{node_id: row.source})
		WITH n, row
		UNWIND row.destinations as dest
		MATCH (m:Process{node_id: dest})
		MERGE (n)-[:HOSTS]->(m)`,
		map[string]interface{}{"batch": batches.Container_process_edges_batch}); err != nil {
		return err
	}

	if _, err = tx.Run(`
		UNWIND $batch as row
		MATCH (n:Node{node_id: row.source})
		WITH n, row
		UNWIND row.destinations as dest
		MATCH (m:Container{node_id: dest})
		MERGE (n)-[:HOSTS]->(m)`,
		map[string]interface{}{"batch": batches.Container_edges_batch}); err != nil {
		return err
	}

	if _, err = tx.Run(`
		UNWIND $batch as row
		MATCH (n:Node{node_id: row.source})
		WITH n, row
		UNWIND row.destinations as dest
		MATCH (m:ContainerImage{node_id: dest})
		MERGE (n)-[:HOSTS]->(m)`,
		map[string]interface{}{"batch": batches.Container_image_edge_batch}); err != nil {
		return err
	}

	if _, err = tx.Run(`
		UNWIND $batch as row
		MATCH (n:KubernetesCluster{node_id: row.source})
		WITH n, row
		UNWIND row.destinations as dest
		MATCH (m:Node{node_id: dest})
		MERGE (n)-[:INSTANCIATE]->(m)`,
		map[string]interface{}{"batch": batches.Kubernetes_cluster_edge_batch}); err != nil {
		return err
	}

	if _, err = tx.Run(`
		UNWIND $batch as row
		MATCH (n:KubernetesCluster{node_id: row.source})
		WITH n, row
		UNWIND row.destinations as dest
		MATCH (m:Pod{node_id: dest})
		MERGE (n)-[:HOSTS]->(m)`,
		map[string]interface{}{"batch": batches.Pod_edges_batch}); err != nil {
		return err
	}

	if _, err = tx.Run(`
		UNWIND $batch as row
		MATCH (n:Node{node_id: row.source})
		WITH n, row
		UNWIND row.destinations as dest
		MATCH (m:Pod{node_id: dest})
		MERGE (n)-[:HOSTS]->(m)`,
		map[string]interface{}{"batch": batches.Pod_host_edges_batch}); err != nil {
		return err
	}

	if _, err = tx.Run(`
		UNWIND $batch as row
		MATCH (n:Node{node_id: row.source})
		WITH n, row
		UNWIND row.destinations as dest
		MATCH (m:Process{node_id: dest})
		MERGE (n)-[:HOSTS]->(m)`,
		map[string]interface{}{"batch": batches.Process_edges_batch}); err != nil {
		return err
	}

	if _, err = tx.Run(`
		UNWIND $batch as row
		MATCH (n:Node{node_id: row.node_id}) -[r:CONNECTS]-> (:Node)
		DELETE r`,
		map[string]interface{}{"batch": batches.Hosts}); err != nil {
		return err
	}

	if _, err = tx.Run(`
		UNWIND $batch as row
		MATCH (n:Node{node_id: 'in-the-internet'}) -[r:CONNECTS]-> (n:Node{node_id: row.node_id})
		DELETE r`, map[string]interface{}{"batch": batches.Hosts}); err != nil {
		return err
	}

	if _, err = tx.Run(`
		UNWIND $batch as row
		MATCH (n:Node{node_id: row.source})
		MATCH (m:Node{node_id: row.destination})
		MERGE (n)-[r:CONNECTS]->(m)
		WITH n, r, m, row.pids as rpids
		UNWIND rpids as pids
		SET r.left_pids = coalesce(r.left_pids, []) + pids.left,
		    r.right_pids = coalesce(r.right_pids, []) + pids.right`, map[string]interface{}{"batch": batches.Endpoint_edges_batch}); err != nil {
		return err
	}

	return tx.Commit()
}

func (nc *neo4jIngester) runIngester() {
	for crpt := range nc.ingester {
		rpt := report.MakeReport()
		if err := crpt.FillReport(&rpt); err != nil {
			log.Error().Msgf("Failed to unmarshal report")
			crpt.Cleanup()
			continue
		}
		crpt.Cleanup()
		nc.preparers_input <- rpt
	}
	log.Info().Msgf("runIngester ended")
}

func (nc *neo4jIngester) runDBBatcher(db_pusher chan ReportIngestionData) {
	batch := [db_batch_size]ReportIngestionData{}
	size := 0
	send := false
	ticker := time.NewTicker(db_batch_timeout)
	defer ticker.Stop()
loop:
	for {
		select {
		case report, open := <-nc.batcher:
			if !open {
				break loop
			}
			batch[size] = report
			size += 1
			send = size == db_batch_size
		case <-ticker.C:
			send = size > 0
		}
		if send {
			if size > 0 {
				send = false
				final_batch := mergeIngestionData(batch[:size])
				select {
				case db_pusher <- final_batch:
				default:
					log.Warn().Msgf("DB channel full")
					breaker.Store(true)
					// Send report & wait
					db_pusher <- final_batch
				}
				size = 0
				batch = [db_batch_size]ReportIngestionData{}
				ticker.Reset(db_batch_timeout)
			}
		}
	}
	log.Info().Msgf("runDBPusher ended")
}

func isTransientError(err error) bool {
	// Check if the error is a deadlock error
	if neoErr, ok := err.(*db.Neo4jError); ok {
		return strings.HasPrefix(neoErr.Code, "Neo.TransientError")
	}
	return false
}

func (nc *neo4jIngester) runDBPusher(db_pusher, db_pusher_retry chan ReportIngestionData) {
	session, err := nc.driver.Session(neo4j.AccessModeWrite)
	if err != nil {
		log.Error().Msgf("Failed to open session: %v", err)
		return
	}
	defer session.Close()

	for batches := range db_pusher {
		span := telemetry.NewSpan(context.Background(), "ingester", "PushAgentReportsToDB")
		err := nc.PushToDB(batches, session)
		if err != nil {
			log.Error().Msgf("push to neo4j err: %v", err)
			span.EndWithErr(err)
			if isTransientError(err) {
				db_pusher_retry <- batches
			}
		} else {
			nc.num_ingested.Add(int32(batches.NumMerged))
			span.End()
		}
	}
	log.Info().Msgf("runDBPusher ended")
}

func (nc *neo4jIngester) runPreparer() {
	var buf bytes.Buffer
	for rpt := range nc.preparers_input {
		r := computeResolvers(&rpt, &buf)
		data := prepareNeo4jIngestion(&rpt, &nc.resolvers, &buf)
		select {
		case nc.batcher <- data:
			nc.resolvers_update <- r
		case nc.resolvers_update <- r:
			nc.batcher <- data
		}
	}
}

func NewNeo4jCollector(ctx context.Context) (Ingester[report.CompressedReport], error) {
	driver, err := directory.Neo4jClient(ctx)

	if err != nil {
		return nil, err
	}

	rdb, err := newEndpointResolversCache(ctx)

	if err != nil {
		return nil, err
	}

	done := make(chan struct{})
	db_pusher := make(chan ReportIngestionData, db_input_size)
	db_pusher_retry := make(chan ReportIngestionData, db_input_size)
	nc := &neo4jIngester{
		driver:           driver,
		resolvers:        rdb,
		ingester:         make(chan report.CompressedReport, ingester_size),
		batcher:          make(chan ReportIngestionData, ingester_size),
		resolvers_update: make(chan EndpointResolvers, db_batch_size),
		preparers_input:  make(chan report.Report, db_batch_size),
		done:             done,
		db_pusher:        db_pusher,
		num_ingested:     atomic.Int32{},
		num_received:     atomic.Int32{},
	}

	for i := 0; i < uncompress_workers_num; i++ {
		go nc.runIngester()
	}

	for i := 0; i < db_input_size; i++ {
		go nc.runDBPusher(db_pusher, db_pusher_retry)
		go nc.runDBPusher(db_pusher_retry, db_pusher)
	}

	go nc.runDBBatcher(db_pusher)
	go nc.resolversUpdater()

	for i := 0; i < preparer_workers_num; i++ {
		go nc.runPreparer()
	}

	go func() {
		// Received num at the time of push back change
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
	loop:
		for {
			select {
			case <-ticker.C:
			case <-done:
				break loop
			}

			log.Info().Msgf("Q: %v, %v, %v, %v, %v, %v",
				len(nc.ingester),
				len(nc.preparers_input),
				len(nc.resolvers_update),
				len(nc.batcher),
				len(nc.db_pusher),
				len(db_pusher_retry))
		}
	}()

	// Push back updater
	go func() {
		// Received num at the time of push back change
		prev_received_num := int32(0)
		newValue, err := GetPushBack(nc.driver)
		if err != nil {
			log.Error().Msgf("Fail to get push back value: %v", err)
		} else {
			Push_back.Store(newValue)
		}
		session, err := driver.Session(neo4j.AccessModeWrite)
		if err != nil {
			log.Error().Msgf("Fail to get session for push back", err)
			return
		}
		defer session.Close()
		ticker := time.NewTicker(agent_base_timeout * time.Duration(Push_back.Load()))
		defer ticker.Stop()
		update_ticker := false
	loop:
		for {
			select {
			case <-ticker.C:
			case <-done:
				break loop
			}

			current_num_received := nc.num_received.Swap(0)
			current_num_ingested := nc.num_ingested.Swap(0)

			toAdd := int32(0)
			if breaker.Swap(false) || current_num_ingested < (current_num_received/4)*3 {
				toAdd = max(current_num_received, 1) / max(current_num_ingested, 1)
				prev_received_num = current_num_received
				update_ticker = true
			} else if current_num_received < (prev_received_num/4)*3 {
				toAdd = -1 * max(prev_received_num, 1) / max(current_num_received, 1)
				if Push_back.Load()+toAdd <= 0 {
					toAdd = -1*Push_back.Load() + 1
				}
				prev_received_num = current_num_received
				update_ticker = true
			}

			// Keep the highest number of reports
			prev_received_num = max(prev_received_num, current_num_received)

			err := UpdatePushBack(session, &Push_back, toAdd)
			if err != nil {
				log.Error().Msgf("push back err: %v", err)
			}

			if update_ticker {
				update_ticker = false
				ticker.Reset(agent_base_timeout * time.Duration(Push_back.Load()))
			}

			log.Info().Msgf("Received: %v, pushed: %v, Push back: %v", current_num_received, current_num_ingested, Push_back.Load())
		}
	}()

	return nc, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int32) int32 {
	if a < b {
		return b
	}
	return a
}

func extractIPPortFromEndpointID(node_id string) (string, string) {
	first := strings.IndexByte(node_id, ';')
	second := strings.IndexByte(node_id[first+1:], ';') + first + 1
	return node_id[first+1 : second], node_id[second+1:]
}

func extractPidFromNodeID(hni string) int {
	middle := strings.IndexByte(hni, ';')
	if middle > 0 {
		num, err := strconv.Atoi(hni[middle+1:])
		if err != nil {
			return 0
		}
		return num
	}
	return 0
}

func metadataToMap(n report.Metadata) map[string]interface{} {
	// convert struct to map
	return utils.StructToMap(n)
}

// TODO: improve syncro
func UpdatePushBack(session neo4j.Session, newValue *atomic.Int32, add int32) error {
	tx, err := session.BeginTransaction()
	if err != nil {
		return err
	}
	defer tx.Close()

	var rec *db.Record
	if add != 0 {
		str := strconv.Itoa(int(newValue.Load() + add))

		res, err := tx.Run(`
			MATCH (n:Node{node_id:"deepfence-console-cron"})
			CALL apoc.atomic.update(n, 'push_back','`+str+`')
			YIELD oldValue, newValue
			return newValue`,
			map[string]interface{}{})
		if err != nil {
			return err
		}
		rec, err = res.Single()
		if err != nil {
			return err
		}
	} else {
		res, err := tx.Run(`
			MATCH (n:Node{node_id:"deepfence-console-cron"})
			RETURN n.push_back`,
			map[string]interface{}{})
		if err != nil {
			return err
		}
		rec, err = res.Single()
		if err != nil {
			return err
		}
	}

	newValue.Store(int32(rec.Values[0].(int64)))

	return tx.Commit()
}

func GetPushBack(driver neo4j.Driver) (int32, error) {
	session, err := driver.Session(neo4j.AccessModeRead)
	if err != nil {
		return 0, err
	}
	defer session.Close()
	tx, err := session.BeginTransaction()
	if err != nil {
		return 0, err
	}
	defer tx.Close()

	res, err := tx.Run(`
		MATCH (n:Node{node_id:"deepfence-console-cron"})
		RETURN n.push_back
		`,
		map[string]interface{}{})
	if err != nil {
		return 0, err
	}
	rec, err := res.Single()
	if err != nil {
		return 0, err
	}
	return int32(rec.Values[0].(int64)), nil
}