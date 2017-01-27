/*  Copyright 2016 Adrian Todorov, Oxalide ato@oxalide.com
	Original project author: https://github.com/cblomart
	This program is free software: you can redistribute it and/or modify
    it under the terms of the GNU General Public License as published by
    the Free Software Foundation, either version 3 of the License, or
    (at your option) any later version.

    This program is distributed in the hope that it will be useful,
    but WITHOUT ANY WARRANTY; without even the implied warranty of
    MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
    GNU General Public License for more details.

    You should have received a copy of the GNU General Public License
    along with this program.  If not, see <http://www.gnu.org/licenses/>.

*/

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/davecgh/go-spew/spew"
	"golang.org/x/net/context"
	"log"
	"math"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	influxclient "github.com/influxdata/influxdb/client/v2"
	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

const (
	// name of the service
	name        = "vsphere-influxdb"
	description = "send vsphere stats to influxdb"
)

// Configuration
type Configuration struct {
	VCenters []*VCenter
	Metrics  []Metric
	Interval int
	Domain   string
	InfluxDB InfluxDB
}

// InfluxDB description
type InfluxDB struct {
	Hostname string
	Username string
	Password string
	Database string
}

// VCenter description
type VCenter struct {
	Hostname     string
	Username     string
	Password     string
	MetricGroups []*MetricGroup
}

// Metric Definition
type MetricDef struct {
	Metric    string
	Instances string
	Key       int32
}

// Metrics description in config
var vm_refs []types.ManagedObjectReference
var debug bool

type Metric struct {
	ObjectType []string
	Definition []MetricDef
}

// Metric Grouping for retrieval
type MetricGroup struct {
	ObjectType string
	Metrics    []MetricDef
	Mor        []types.ManagedObjectReference
}

// Informations to query about an entity
type EntityQuery struct {
	Name    string
	Entity  types.ManagedObjectReference
	Metrics []int32
}

// A few global variables
var dependencies = []string{}

var stdlog, errlog *log.Logger

func (vcenter *VCenter) Connect() (*govmomi.Client, error) {
	// Prepare vCenter Connections
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdlog.Println("connecting to vcenter: " + vcenter.Hostname)
	u, err := url.Parse("https://" + vcenter.Username + ":" + vcenter.Password + "@" + vcenter.Hostname + "/sdk")
	if err != nil {
		errlog.Println("Could not parse vcenter url: ", vcenter.Hostname)
		errlog.Println("Error: ", err)
		return nil, err
	}
	client, err := govmomi.NewClient(ctx, u, true)
	if err != nil {
		errlog.Println("Could not connect to vcenter: ", vcenter.Hostname)
		errlog.Println("Error: ", err)
		return nil, err
	}
	return client, nil
}

// Initialise vcenter
func (vcenter *VCenter) Init(config Configuration) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client, err := vcenter.Connect()
	if err != nil {
		errlog.Println("Could not connect to vcenter: ", vcenter.Hostname)
		errlog.Println("Error: ", err)
		return
	}
	defer client.Logout(ctx)
	var perfmanager mo.PerformanceManager
	err = client.RetrieveOne(ctx, *client.ServiceContent.PerfManager, nil, &perfmanager)
	if err != nil {
		errlog.Println("Could not get performance manager")
		errlog.Println("Error: ", err)
		return
	}
	for _, perf := range perfmanager.PerfCounter {
		groupinfo := perf.GroupInfo.GetElementDescription()
		nameinfo := perf.NameInfo.GetElementDescription()
		identifier := groupinfo.Key + "." + nameinfo.Key + "." + fmt.Sprint(perf.RollupType)
		for _, metric := range config.Metrics {
			for _, metricdef := range metric.Definition {
				if metricdef.Metric == identifier {
					metricd := MetricDef{Metric: metricdef.Metric, Instances: metricdef.Instances, Key: perf.Key}
					for _, mtype := range metric.ObjectType {
						added := false
						for _, metricgroup := range vcenter.MetricGroups {
							if metricgroup.ObjectType == mtype {
								metricgroup.Metrics = append(metricgroup.Metrics, metricd)
								added = true
								break
							}
						}
						if added == false {
							metricgroup := MetricGroup{ObjectType: mtype, Metrics: []MetricDef{metricd}}
							vcenter.MetricGroups = append(vcenter.MetricGroups, &metricgroup)
						}
					}
				}
			}
		}
	}
}

// Query a vcenter
func (vcenter *VCenter) Query(config Configuration, InfluxDBClient influxclient.Client) {
	stdlog.Println("Setting up query inventory of vcenter: ", vcenter.Hostname)

	// Create the contect
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Get the client
	client, err := vcenter.Connect()
	if err != nil {
		errlog.Println("Could not connect to vcenter: ", vcenter.Hostname)
		errlog.Println("Error: ", err)
		return
	}
	defer client.Logout(ctx)

	// Create the view manager
	var viewManager mo.ViewManager
	err = client.RetrieveOne(ctx, *client.ServiceContent.ViewManager, nil, &viewManager)
	if err != nil {
		errlog.Println("Could not get view manager from vcenter: " + vcenter.Hostname)
		errlog.Println("Error: ", err)
		return
	}

	// Get the Datacenters from root folder
	var rootFolder mo.Folder
	err = client.RetrieveOne(ctx, client.ServiceContent.RootFolder, nil, &rootFolder)
	if err != nil {
		errlog.Println("Could not get root folder from vcenter: " + vcenter.Hostname)
		errlog.Println("Error: ", err)
		return
	}

	datacenters := []types.ManagedObjectReference{}
	for _, child := range rootFolder.ChildEntity {
		if child.Type == "Datacenter" {
			datacenters = append(datacenters, child)
		}
	}
	// Get intresting object types from specified queries
	objectTypes := []string{}
	for _, group := range vcenter.MetricGroups {
		objectTypes = append(objectTypes, group.ObjectType)
	}
	objectTypes = append(objectTypes, "ClusterComputeResource")

	// Loop trought datacenters and create the intersting object reference list
	mors := []types.ManagedObjectReference{}
	for _, datacenter := range datacenters {
		// Create the CreateContentView request
		req := types.CreateContainerView{This: viewManager.Reference(), Container: datacenter, Type: objectTypes, Recursive: true}
		res, err := methods.CreateContainerView(ctx, client.RoundTripper, &req)
		if err != nil {
			errlog.Println("Could not create container view from vcenter: " + vcenter.Hostname)
			errlog.Println("Error: ", err)
			continue
		}
		// Retrieve the created ContentView
		var containerView mo.ContainerView
		err = client.RetrieveOne(ctx, res.Returnval, nil, &containerView)
		if err != nil {
			errlog.Println("Could not get container view from vcenter: " + vcenter.Hostname)
			errlog.Println("Error: ", err)
			continue
		}
		// Add found object to object list
		mors = append(mors, containerView.View...)
	}
	// Create MORS for each object type
	vm_refs := []types.ManagedObjectReference{}
	host_refs := []types.ManagedObjectReference{}
	cluster_refs := []types.ManagedObjectReference{}

	new_mors := []types.ManagedObjectReference{}

	spew.Dump(mors)
	// Assign each MORS type to a specific array
	for _, mor := range mors {
		if mor.Type == "VirtualMachine" {
			vm_refs = append(vm_refs, mor)
			new_mors = append(new_mors, mor)
		} else if mor.Type == "HostSystem" {
			host_refs = append(host_refs, mor)
			new_mors = append(new_mors, mor)
		} else if mor.Type == "ClusterComputeResource" {
			cluster_refs = append(cluster_refs, mor)
		}
	}
	// Copy  the mors without the clusters
	mors = new_mors

	pc := property.DefaultCollector(client.Client)

	// Retrieve properties for all vms
	var vmmo []mo.VirtualMachine
	if len(vm_refs) > 0 {
		err = pc.Retrieve(ctx, vm_refs, []string{"summary"}, &vmmo)
		if err != nil {
			fmt.Println(err)
			return
		}
	}

	// Retrieve properties for hosts
	var hsmo []mo.HostSystem
	if len(host_refs) > 0 {
		err = pc.Retrieve(ctx, host_refs, []string{"summary"}, &hsmo)
		if err != nil {
			fmt.Println(err)
			return
		}
	}

	// Initialize the map that will hold the VM MOR to cluster reference
	vmToCluster := make(map[types.ManagedObjectReference]string)

	// Retrieve properties for clusters, if any
	if len(cluster_refs) > 0 {
		if debug == true {
			stdlog.Println("going inside clusters")
		}
		var clmo []mo.ClusterComputeResource
		err = pc.Retrieve(ctx, cluster_refs, []string{"name", "configuration"}, &clmo)
		if err != nil {
			fmt.Println(err)
			return
		}
		for _, cl := range clmo {
			if debug == true {
				stdlog.Println("---cluster name - you should see every cluster here---")
				stdlog.Println(cl.Name)
				stdlog.Println("You should see the cluster object, clsuter configuration object, and cluster configuration dasvmconfig which should contain all VMs")
				spew.Dump(cl)
				spew.Dump(cl.Configuration)
				spew.Dump(cl.Configuration.DasVmConfig)
			}
			for _, vm := range cl.Configuration.DasVmConfig {
				if debug == true {
					stdlog.Println("--VM ID - you should see every VM ID here--")
					stdlog.Println(vm.Key.Value)
				}

				vmToCluster[vm.Key] = cl.Name
			}
		}
	}

	// Retrieve properties for the hosts
	host_summary := make(map[types.ManagedObjectReference]map[string]string)
	host_extra_metrics := make(map[types.ManagedObjectReference]map[string]int64)

	for _, host := range hsmo {
		host_summary[host.Self] = make(map[string]string)
		host_summary[host.Self]["name"] = host.Summary.Config.Name
		host_extra_metrics[host.Self] = make(map[string]int64)
		host_extra_metrics[host.Self]["cpu_corecount_total"] = int64(host.Summary.Hardware.NumCpuThreads)
	}

	// Initialize the map that will hold all extra tags
	vm_summary := make(map[types.ManagedObjectReference]map[string]string)

	// Assign extra details per VM in vm_summary
	for _, vm := range vmmo {
		vm_summary[vm.Self] = make(map[string]string)
		// Ugly way to extract datastore value
		re, err := regexp.Compile(`\[(.*?)\]`)
		if err != nil {
			fmt.Println(err)
		}
		vm_summary[vm.Self]["datastore"] = strings.Replace(strings.Replace(re.FindString(fmt.Sprintln(vm.Summary.Config)), "[", "", -1), "]", "", -1)
		if vmToCluster[vm.Self] != "" {
			vm_summary[vm.Self]["cluster"] = vmToCluster[vm.Self]
		}
		vm_summary[vm.Self]["esx"] = host_summary[*vm.Summary.Runtime.Host]["name"]
	}

	// get object names
	objects := []mo.ManagedEntity{}

	//object for propery collection
	propSpec := &types.PropertySpec{Type: "ManagedEntity", PathSet: []string{"name"}}
	var objectSet []types.ObjectSpec
	for _, mor := range mors {
		objectSet = append(objectSet, types.ObjectSpec{Obj: mor, Skip: types.NewBool(false)})
	}

	//retrieve name property
	propreq := types.RetrieveProperties{SpecSet: []types.PropertyFilterSpec{{ObjectSet: objectSet, PropSet: []types.PropertySpec{*propSpec}}}}
	propres, err := client.PropertyCollector().RetrieveProperties(ctx, propreq)
	if err != nil {
		errlog.Println("Could not retrieve object names from vcenter: " + vcenter.Hostname)
		errlog.Println("Error: ", err)
		return
	}

	//load retrieved properties
	err = mo.LoadRetrievePropertiesResponse(propres, &objects)
	if err != nil {
		errlog.Println("Could not retrieve object names from vcenter: " + vcenter.Hostname)
		errlog.Println("Error: ", err)
		return
	}

	//create a map to resolve object names
	morToName := make(map[types.ManagedObjectReference]string)
	for _, object := range objects {
		morToName[object.Self] = object.Name
	}

	//create a map to resolve metric names
	metricToName := make(map[int32]string)
	for _, metricgroup := range vcenter.MetricGroups {
		for _, metricdef := range metricgroup.Metrics {
			metricToName[metricdef.Key] = metricdef.Metric
		}
	}

	// Create Queries from interesting objects and requested metrics

	queries := []types.PerfQuerySpec{}

	// Common parameters
	intervalIdint := 20
	var intervalId int32
	intervalId = int32(intervalIdint)

	endTime := time.Now().Add(time.Duration(-1) * time.Second)
	startTime := endTime.Add(time.Duration(-config.Interval) * time.Second)

	// Parse objects
	for _, mor := range mors {
		metricIds := []types.PerfMetricId{}
		for _, metricgroup := range vcenter.MetricGroups {
			if metricgroup.ObjectType == mor.Type {
				for _, metricdef := range metricgroup.Metrics {
					metricIds = append(metricIds, types.PerfMetricId{CounterId: metricdef.Key, Instance: metricdef.Instances})
				}
			}
		}
		queries = append(queries, types.PerfQuerySpec{Entity: mor, StartTime: &startTime, EndTime: &endTime, MetricId: metricIds, IntervalId: intervalId})
	}

	// Query the performances
	perfreq := types.QueryPerf{This: *client.ServiceContent.PerfManager, QuerySpec: queries}
	perfres, err := methods.QueryPerf(ctx, client.RoundTripper, &perfreq)
	if err != nil {
		errlog.Println("Could not request perfs from vcenter: " + vcenter.Hostname)
		errlog.Println("Error: ", err)
		return
	}

	// Get the result
	vcName := strings.Replace(vcenter.Hostname, config.Domain, "", -1)

	//Influx batch points
	bp, err := influxclient.NewBatchPoints(influxclient.BatchPointsConfig{
		Database:  config.InfluxDB.Database,
		Precision: "s",
	})
	if err != nil {
		errlog.Println(err)
	}

	for _, base := range perfres.Returnval {
		pem := base.(*types.PerfEntityMetric)
		entityName := strings.ToLower(pem.Entity.Type)
		name := strings.ToLower(strings.Replace(morToName[pem.Entity], config.Domain, "", -1))

		//Create map for InfluxDB fields
		fields := make(map[string]interface{})

		// Create map for InfluxDB tags
		tags := map[string]string{"host": vcName, "name": name}

		// Add extra per VM tags
		if summary, ok := vm_summary[pem.Entity]; ok {
			for key, tag := range summary {
				tags[key] = tag
			}
		}
		if summary, ok := host_summary[pem.Entity]; ok {
			for key, tag := range summary {
				tags[key] = tag
			}
		}

		special_fields := make(map[string]map[string]map[string]map[string]interface{})
		special_tags := make(map[string]map[string]map[string]map[string]string)
		nowTime := time.Now()
		for _, baseserie := range pem.Value {
			serie := baseserie.(*types.PerfMetricIntSeries)
			metricName := strings.ToLower(metricToName[serie.Id.CounterId])
			influxMetricName := strings.Replace(metricName, ".", "_", -1)
			instanceName := strings.ToLower(strings.Replace(serie.Id.Instance, ".", "_", -1))
			measurementName := strings.Split(metricName, ".")[0]

			if strings.Index(influxMetricName, "datastore") != -1 {
				instanceName = ""
			}

			var value int64 = -1
			if strings.HasSuffix(metricName, ".average") {
				value = average(serie.Value...)
			} else if strings.HasSuffix(metricName, ".maximum") {
				value = max(serie.Value...)
			} else if strings.HasSuffix(metricName, ".minimum") {
				value = min(serie.Value...)
			} else if strings.HasSuffix(metricName, ".latest") {
				value = serie.Value[len(serie.Value)-1]
			} else if strings.HasSuffix(metricName, ".summation") {
				value = sum(serie.Value...)
			}

			if instanceName == "" {
				fields[influxMetricName] = value
			} else {
				// init maps
				if special_fields[measurementName] == nil {
					special_fields[measurementName] = make(map[string]map[string]map[string]interface{})
					special_tags[measurementName] = make(map[string]map[string]map[string]string)

				}

				if special_fields[measurementName][tags["name"]] == nil {
					special_fields[measurementName][tags["name"]] = make(map[string]map[string]interface{})
					special_tags[measurementName][tags["name"]] = make(map[string]map[string]string)
				}

				if special_fields[measurementName][tags["name"]][instanceName] == nil {
					special_fields[measurementName][tags["name"]][instanceName] = make(map[string]interface{})
					special_tags[measurementName][tags["name"]][instanceName] = make(map[string]string)

				}

				special_fields[measurementName][tags["name"]][instanceName][influxMetricName] = value

				for k, v := range tags {
					special_tags[measurementName][tags["name"]][instanceName][k] = v
				}
				special_tags[measurementName][tags["name"]][instanceName]["instance"] = instanceName

			}
		}

		if metrics, ok := host_extra_metrics[pem.Entity]; ok {
			for key, value := range metrics {
				fields[key] = value
			}
		}

		//create InfluxDB points
		pt, err := influxclient.NewPoint(entityName, tags, fields, nowTime)
		if err != nil {
			errlog.Println(err)
		}
		bp.AddPoint(pt)

		for measurement, v := range special_fields {
			for name, metric := range v {
				for instance, value := range metric {
					pt2, err := influxclient.NewPoint(measurement, special_tags[measurement][name][instance], value, time.Now())
					if err != nil {
						errlog.Println(err)
					}
					bp.AddPoint(pt2)
				}
			}
		}
	}
	//InfluxDB send
	err = InfluxDBClient.Write(bp)
	if err != nil {
		errlog.Println(err)
	} else {
		stdlog.Println("sent data to Influxdb")
	}

}

func min(n ...int64) int64 {
	var min int64 = -1
	for _, i := range n {
		if i >= 0 {
			if min == -1 {
				min = i
			} else {
				if i < min {
					min = i
				}
			}
		}
	}
	return min
}

func max(n ...int64) int64 {
	var max int64 = -1
	for _, i := range n {
		if i >= 0 {
			if max == -1 {
				max = i
			} else {
				if i > max {
					max = i
				}
			}
		}
	}
	return max
}

func sum(n ...int64) int64 {
	var total int64 = 0
	for _, i := range n {
		if i > 0 {
			total += i
		}
	}
	return total
}

func average(n ...int64) int64 {
	var total int64 = 0
	var count int64 = 0
	for _, i := range n {
		if i >= 0 {
			count += 1
			total += i
		}
	}
	favg := float64(total) / float64(count)
	return int64(math.Floor(favg + .5))
}

func queryVCenter(vcenter VCenter, config Configuration, InfluxDBClient influxclient.Client) {
	stdlog.Println("Querying vcenter")
	vcenter.Query(config, InfluxDBClient)

}

func main() {

	flag.BoolVar(&debug, "debug", false, "Debug mode")
	flag.Parse()

	stdlog = log.New(os.Stdout, "", log.Ldate|log.Ltime)
	errlog = log.New(os.Stderr, "", log.Ldate|log.Ltime)

	stdlog.Println("Starting :", path.Base(os.Args[0]))
	// read the configuration
	file, err := os.Open("/etc/" + path.Base(os.Args[0]) + ".json")
	if err != nil {
		errlog.Println("Could not open configuration file")
		errlog.Println(err)
	}
	jsondec := json.NewDecoder(file)
	config := Configuration{}
	err = jsondec.Decode(&config)
	if err != nil {
		errlog.Println("Could not decode configuration file")
		errlog.Println(err)

	}
	for _, vcenter := range config.VCenters {
		vcenter.Init(config)
	}
	InfluxDBClient, err := influxclient.NewHTTPClient(influxclient.HTTPConfig{
		Addr:     config.InfluxDB.Hostname,
		Username: config.InfluxDB.Username,
		Password: config.InfluxDB.Password,
	})
	if err != nil {
		errlog.Println("Could not connect to InfluxDB")
		errlog.Println(err)
	} else {
		stdlog.Println("Successfully connected to Influx\n")
	}
	for _, vcenter := range config.VCenters {
		queryVCenter(*vcenter, config, InfluxDBClient)
	}
}
