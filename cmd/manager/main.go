package main

import (
	"context"
	"flag"
	"fmt"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"os"
	"runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"strings"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/rest"

	apis "github.com/kubeflow/kfctl/v3/pkg/apis/apps"
	"github.com/kubeflow/kfctl/v3/pkg/controller"
	kfdefcontroller "github.com/kubeflow/kfctl/v3/pkg/controller/kfdef"

	"github.com/operator-framework/operator-sdk/pkg/k8sutil"
	kubemetrics "github.com/operator-framework/operator-sdk/pkg/kube-metrics"
	"github.com/operator-framework/operator-sdk/pkg/leader"
	"github.com/operator-framework/operator-sdk/pkg/log/zap"
	"github.com/operator-framework/operator-sdk/pkg/metrics"
	"github.com/operator-framework/operator-sdk/pkg/restmapper"
	sdkVersion "github.com/operator-framework/operator-sdk/version"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
)

// Kubeflow operator version
var (
	Version string = "1.1.0"
)

// Change below variables to serve metrics on different host or port.
var (
	metricsHost               = "0.0.0.0"
	metricsPort         int32 = 8383
	operatorMetricsPort int32 = 8686
)

func printVersion() {
	log.Infof("Go Version: %s", runtime.Version())
	log.Infof("Go OS/Arch: %s/%s", runtime.GOOS, runtime.GOARCH)
	log.Infof("Version of operator-sdk: %v", sdkVersion.Version)
	log.Infof("Kubeflow version: %v", Version)
}

func main() {
	// Add the zap logger flag set to the CLI. The flag set must
	// be added before calling pflag.Parse().
	pflag.CommandLine.AddFlagSet(zap.FlagSet())

	// Add flags registered by imported packages (e.g. glog and
	// controller-runtime)
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)

	pflag.Parse()

	printVersion()

	watchNamespace, err := k8sutil.GetWatchNamespace()
	if err != nil {
		log.Warnf("Failed to get watch watchNamespace. "+
			"The manager will watch and manage resources in all Namespaces. "+
			"Error %v.", err)
	}

	// Get a config to talk to the apiserver
	cfg, err := config.GetConfig()
	if err != nil {
		log.Errorf("Error: %v.", err)
		os.Exit(1)
	}

	ctx := context.TODO()
	// Become the leader before proceeding
	err = leader.Become(ctx, "kfctl-lock")
	if err != nil {
		log.Errorf("Error: %v.", err)
		os.Exit(1)
	}

	options := manager.Options{
		Namespace:          watchNamespace, //"" will watch all namespaces
		MapperProvider:     restmapper.NewDynamicRESTMapper,
		MetricsBindAddress: fmt.Sprintf("%s:%d", metricsHost, metricsPort),
	}

	// MultiNamespace set in WATCH_NAMESPACE (e.g ns1,ns2)
	if strings.Contains(watchNamespace, ",") {
		log.Infof("manager set up with multiple namespaces: %s", watchNamespace)
		// configure cluster-scoped with MultiNamespacedCacheBuilder
		options.Namespace = ""
		options.NewCache = cache.MultiNamespacedCacheBuilder(strings.Split(watchNamespace, ","))
	}

	// Create a new Cmd to provide shared dependencies and start components
	mgr, err := manager.New(cfg, options)
	if err != nil {
		log.Errorf("Error: %v.", err)
		os.Exit(1)
	}

	log.Info("Registering Components.")

	// Setup Scheme for all resources
	if err := apis.AddToScheme(mgr.GetScheme()); err != nil {
		log.Errorf("Error: %v.", err)
		os.Exit(1)
	}

	// Setup all Controllers
	if err := controller.AddToManager(mgr); err != nil {
		log.Errorf("Error: %v.", err)
		os.Exit(1)
	}

	if err = serveCRMetrics(cfg); err != nil {
		log.Errorf("Could not generate and serve custom resource metrics. Error: %v.", err.Error())
	}

	// Add to the below struct any other metrics ports you want to expose.
	servicePorts := []v1.ServicePort{
		{Port: metricsPort, Name: metrics.OperatorPortName, Protocol: v1.ProtocolTCP, TargetPort: intstr.IntOrString{Type: intstr.Int, IntVal: metricsPort}},
		{Port: operatorMetricsPort, Name: metrics.CRPortName, Protocol: v1.ProtocolTCP, TargetPort: intstr.IntOrString{Type: intstr.Int, IntVal: operatorMetricsPort}},
	}
	// Create Service object to expose the metrics port(s).
	service, err := metrics.CreateMetricsService(ctx, cfg, servicePorts)
	if err != nil {
		log.Errorf("Could not create metrics Service. Error: %v.", err.Error())
	}

	// CreateServiceMonitors will automatically create the prometheus-operator ServiceMonitor resources
	// necessary to configure Prometheus to scrape metrics from this operator.
	operatorNamespace, _ := k8sutil.GetOperatorNamespace()
	services := []*v1.Service{service}
	_, err = metrics.CreateServiceMonitors(cfg, operatorNamespace, services)
	if err != nil {
		log.Errorf("Could not create ServiceMonitor object. Error: %v.", err.Error())
		// If this operator is deployed to a cluster without the prometheus-operator running, it will return
		// ErrServiceMonitorNotPresent, which can be used to safely skip ServiceMonitor creation.
		if err == metrics.ErrServiceMonitorNotPresent {
			log.Errorf("Install prometheus-operator in your cluster to create ServiceMonitor objects. Error: %v.", err.Error())
		}
	}

	log.Infof("Starting the Cmd.")

	// Start the Cmd
	if err := mgr.Start(signals.SetupSignalHandler()); err != nil {
		log.Errorf("Manager exited non-zero. Error: %v.", err)
		os.Exit(1)
	}
}

// serveCRMetrics gets the Operator/CustomResource GVKs and generates metrics based on those types.
// It serves those metrics on "http://metricsHost:operatorMetricsPort".
func serveCRMetrics(cfg *rest.Config) error {
	// Below function returns filtered operator/CustomResource specific GVKs.
	// For more control override the below GVK list with your own custom logic.
	//filteredGVK, err := k8sutil.GetGVKsFromAddToScheme(apis.AddToScheme)
	gvks, err := k8sutil.GetGVKsFromAddToScheme(apis.AddToScheme)
	if err != nil {
		return err
	}
	// Get the namespace the operator is currently deployed in.
	operatorNs, err := k8sutil.GetOperatorNamespace()
	if err != nil {
		return err
	}

	// Perform custom gvk filtering
	filteredGVK := filterGKVsFromAddToScheme(gvks)
	if err != nil {
		return err
	}

	// To generate metrics in other namespaces, add the values below.
	ns := []string{operatorNs}
	// Generate and serve custom resource specific metrics.
	err = kubemetrics.GenerateAndServeCRMetrics(cfg, ns, filteredGVK, metricsHost, operatorMetricsPort)
	if err != nil {
		return err
	}
	return nil
}

// Reference Issue: https://github.com/operator-framework/operator-sdk/issues/2807#issuecomment-611586550
// For this version of operator-sdk, kube-metrics  lists all of the defined Kinds in the schemas
// that are passed, including Kinds that the operator doesn't use. This function filters the Kinds
// that are watched by the operator.
// Note: This issue was resolved in the later versions of the sdk
func filterGKVsFromAddToScheme(gvks []schema.GroupVersionKind) []schema.GroupVersionKind {
	matchAnyValue := "*"

	ownGVKs := []schema.GroupVersionKind{}
	for _, gvk := range gvks {
		for _, watchedGVK := range kfdefcontroller.WatchedResources {
			match := true
			if watchedGVK.Kind == matchAnyValue && watchedGVK.Group == matchAnyValue && watchedGVK.Version == matchAnyValue {
				match = false
			} else {
				if watchedGVK.Kind != matchAnyValue && watchedGVK.Kind != gvk.Kind {
					match = false
				}
				if watchedGVK.Group != matchAnyValue && watchedGVK.Group != gvk.Group {
					match = false
				}
				if watchedGVK.Version != matchAnyValue && watchedGVK.Version != gvk.Version {
					match = false
				}
			}
			if match {
				ownGVKs = append(ownGVKs, gvk)
			}
		}
	}
	return ownGVKs
}
