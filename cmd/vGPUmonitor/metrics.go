package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/NVIDIA/gpu-monitoring-tools/bindings/go/nvml"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// ClusterManager is an example for a system that might have been built without
// Prometheus in mind. It models a central manager of jobs running in a
// cluster. Thus, we implement a custom Collector called
// ClusterManagerCollector, which collects information from a ClusterManager
// using its provided methods and turns them into Prometheus Metrics for
// collection.
//
// An additional challenge is that multiple instances of the ClusterManager are
// run within the same binary, each in charge of a different zone. We need to
// make use of wrapping Registerers to be able to register each
// ClusterManagerCollector instance with Prometheus.
type ClusterManager struct {
	Zone string
	// Contains many more fields not listed in this example.
}

// ReallyExpensiveAssessmentOfTheSystemState is a mock for the data gathering a
// real cluster manager would have to do. Since it may actually be really
// expensive, it must only be called once per collection. This implementation,
// obviously, only returns some made-up data.
func (c *ClusterManager) ReallyExpensiveAssessmentOfTheSystemState() (
	oomCountByHost map[string]int, ramUsageByHost map[string]float64,
) {
	// Just example fake data.
	oomCountByHost = map[string]int{
		"foo.example.org": 42,
		"bar.example.org": 2001,
	}
	ramUsageByHost = map[string]float64{
		"foo.example.org": 6.023e23,
		"bar.example.org": 3.14,
	}
	return
}

// ClusterManagerCollector implements the Collector interface.
type ClusterManagerCollector struct {
	ClusterManager *ClusterManager
}

// Descriptors used by the ClusterManagerCollector below.
var (
	hostGPUdesc = prometheus.NewDesc(
		"HostGPUMemoryUsage",
		"GPU device memory usage",
		[]string{"deviceid", "deviceuuid"}, nil,
	)

	hostGPUUtilizationdesc = prometheus.NewDesc(
		"HostCoreUtilization",
		"GPU core utilization",
		[]string{"deviceid", "deviceuuid"}, nil,
	)

	ctrvGPUdesc = prometheus.NewDesc(
		"vGPU_device_memory_usage_in_bytes",
		"vGPU device usage",
		[]string{"podnamespace", "podname", "ctrname", "vdeviceid", "deviceuuid"}, nil,
	)

	ctrvGPUlimitdesc = prometheus.NewDesc(
		"vGPU_device_memory_limit_in_bytes",
		"vGPU device limit",
		[]string{"podnamespace", "podname", "ctrname", "vdeviceid", "deviceuuid"}, nil,
	)
	clientset *kubernetes.Clientset
)

// Describe is implemented with DescribeByCollect. That's possible because the
// Collect method will always return the same two metrics with the same two
// descriptors.
func (cc ClusterManagerCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- hostGPUdesc
	ch <- ctrvGPUdesc
	ch <- ctrvGPUlimitdesc
	ch <- hostGPUUtilizationdesc
	//prometheus.DescribeByCollect(cc, ch)
}

func parseidstr(podusage string) (string, string, error) {
	tmp := strings.Split(podusage, "_")
	if len(tmp) > 1 {
		return tmp[0], tmp[1], nil
	} else {
		return "", "", errors.New("parse error")
	}
}

func gettotalusage(usage podusage, vidx int) (uint64, error) {
	var added uint64
	added = 0
	for _, val := range usage.sr.procs {
		added += uint64(val.used[vidx])
	}
	return added, nil
}

// Collect first triggers the ReallyExpensiveAssessmentOfTheSystemState. Then it
// creates constant metrics for each host on the fly based on the returned data.
//
// Note that Collect could be called concurrently, so we depend on
// ReallyExpensiveAssessmentOfTheSystemState to be concurrency-safe.
func (cc ClusterManagerCollector) Collect(ch chan<- prometheus.Metric) {
	fmt.Println("begin collect")
	srlist, err := monitorpath()
	fmt.Println("Collect", srlist)
	if err != nil {
		fmt.Println("err=", err.Error())
	}
	if clientset != nil {
		err := nvml.Init()
		if err != nil {
			fmt.Println("nvml Init err=", err.Error())
		}
		devnum, err := nvml.GetDeviceCount()
		var ii uint
		if err != nil {
			fmt.Println("nvml GetDeviceCount err=", err.Error())
		} else {
			for ii = 0; ii < devnum; ii++ {
				hdev, err := nvml.NewDevice(ii)
				if err != nil {
					fmt.Println(err.Error())
				}
				hstatus, err := hdev.Status()
				if err != nil {
					fmt.Println("hstatus error", err.Error())
					continue
				}
				if hstatus.Memory.Global.Used != nil {
					ch <- prometheus.MustNewConstMetric(
						hostGPUdesc,
						prometheus.GaugeValue,
						float64(*hstatus.Memory.Global.Used),
						fmt.Sprint(ii), hdev.UUID,
					)
				}
				if hstatus.Utilization.GPU != nil {
					ch <- prometheus.MustNewConstMetric(
						hostGPUUtilizationdesc,
						prometheus.GaugeValue,
						float64(*hstatus.Utilization.GPU),
						fmt.Sprint(ii), hdev.UUID,
					)
				}
			}
		}

		pods, err := clientset.CoreV1().Pods("").List(context.TODO(), v1.ListOptions{})
		if err != nil {
			fmt.Println("err=", err.Error())
		}
		for _, val := range pods.Items {
			for _, sr := range srlist {
				pod_uid := strings.Split(sr.idstr, "_")[0]
				ctr_name := strings.Split(sr.idstr, "_")[1]
				fmt.Println("compareing", val.UID, pod_uid)
				if strings.Compare(string(val.UID), pod_uid) == 0 {
					fmt.Println("Pod matched!", val.Name, val.Namespace, val.Labels)
					for _, ctr := range val.Spec.Containers {
						if strings.Compare(ctr.Name, ctr_name) == 0 {
							fmt.Println("container matched", ctr.Name)
							podlabels := make(map[string]string)
							for idx, val := range val.Labels {
								idxfix := strings.ReplaceAll(idx, "-", "_")
								valfix := strings.ReplaceAll(val, "-", "_")
								podlabels[idxfix] = valfix
							}
							for i := 0; i < int(sr.sr.num); i++ {
								value, _ := gettotalusage(sr, i)
								uuid := string(sr.sr.uuids[i].uuid[:])[0:40]

								//fmt.Println("uuid=", uuid, "length=", len(uuid))
								ch <- prometheus.MustNewConstMetric(
									ctrvGPUdesc,
									prometheus.GaugeValue,
									float64(value),
									val.Namespace, val.Name, ctr_name, fmt.Sprint(i), uuid, /*,string(sr.sr.uuids[i].uuid[:])*/
								)
								ch <- prometheus.MustNewConstMetric(
									ctrvGPUlimitdesc,
									prometheus.GaugeValue,
									float64(sr.sr.limit[i]),
									val.Namespace, val.Name, ctr_name, fmt.Sprint(i), uuid, /*,string(sr.sr.uuids[i].uuid[:])*/
								)
							}
						}
					}
				}
			}
		}
	}
}

// NewClusterManager first creates a Prometheus-ignorant ClusterManager
// instance. Then, it creates a ClusterManagerCollector for the just created
// ClusterManager. Finally, it registers the ClusterManagerCollector with a
// wrapping Registerer that adds the zone as a label. In this way, the metrics
// collected by different ClusterManagerCollectors do not collide.
func NewClusterManager(zone string, reg prometheus.Registerer) *ClusterManager {
	c := &ClusterManager{
		Zone: zone,
	}
	cc := ClusterManagerCollector{ClusterManager: c}
	prometheus.WrapRegistererWith(prometheus.Labels{"zone": zone}, reg).MustRegister(cc)
	return c
}

func initmetrics() {
	// Since we are dealing with custom Collector implementations, it might
	// be a good idea to try it out with a pedantic registry.
	fmt.Println("Initializing metrics...")

	reg := prometheus.NewRegistry()
	//reg := prometheus.NewPedanticRegistry()
	config, err := rest.InClusterConfig()
	if err != nil {
		fmt.Println(err.Error())
		return
	}
	clientset, err = kubernetes.NewForConfig(config)
	if err != nil {
		fmt.Println(err.Error())
		return
	}

	// Construct cluster managers. In real code, we would assign them to
	// variables to then do something with them.
	NewClusterManager("vGPU", reg)
	//NewClusterManager("ca", reg)

	// Add the standard process and Go metrics to the custom registry.
	//reg.MustRegister(
	//	prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}),
	//	prometheus.NewGoCollector(),
	//)

	http.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	log.Fatal(http.ListenAndServe(":9394", nil))
}
