package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"k8s.io/client-go/rest"

	"github.com/r3labs/diff"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/viper"
	coreV1 "k8s.io/api/core/v1"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

var clientset *kubernetes.Clientset

const rkeExternalIPAnnotation = "rke.cattle.io/external-ip"
const flannelPublicIPOverrideAnnotation = "flannel.alpha.coreos.com/public-ip-overwrite"
const flannelPublicIPAnnotation = "flannel.alpha.coreos.com/public-ip"

func bootstrap() {
	zerolog.SetGlobalLevel(zerolog.DebugLevel)
	zerolog.TimeFieldFormat = time.RFC3339Nano
	log.Logger = log.Output(zerolog.ConsoleWriter{
		Out:        os.Stderr,
		TimeFormat: "15:04:05",
	})

	viper.AutomaticEnv()
	viper.SetEnvPrefix("FFIXER")
	viper.SetDefault("use_kubeconfig", false)
	viper.SetDefault("kubeconfig", filepath.Join(os.Getenv("HOME"), ".kube", "config"))

	var config *rest.Config
	var err error
	switch {
	case viper.GetBool("use_kubeconfig"):
		log.Debug().Msg("using kubeconfig configuration")
		config, err = clientcmd.BuildConfigFromFlags("", viper.GetString("kubeconfig"))
		if err != nil {
			log.Fatal().Err(err).Msg("cannot build kubeconfig configuration")
		}
	default:
		config, err = rest.InClusterConfig()
		if err != nil {
			log.Fatal().Err(err).Msg("cannot build kubeconfig configuration from cluster role")
		}
	}
	clientset, err = kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatal().Err(err).Msg("cannot connect to cluster")
	}
	http.Handle("/metrics", promhttp.Handler())
	go func() {
		_ = http.ListenAndServe(":2112", nil)
	}()
}
func main() {
	bootstrap()

	watcher := cache.NewListWatchFromClient(clientset.CoreV1().RESTClient(), "nodes", metaV1.NamespaceAll, fields.Everything())
	_, controller := cache.NewInformer(
		watcher,
		&coreV1.Node{},
		0,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				node, ok := obj.(*coreV1.Node)
				if !ok {
					log.Fatal().
						Str("object_type", fmt.Sprintf("%T", node)).
						Msg("list/watch returned non-node object")
				}
				log.Info().Str("node_name", node.Name).Msg("checking node")
				updateNode(node)
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				node, ok := newObj.(*coreV1.Node)
				if !ok {
					log.Fatal().
						Str("object_type", fmt.Sprintf("%T", node)).
						Msg("list/watch update returned non-node object")
				}
				if viper.GetBool("debug") {
					diffs, _ := diff.Diff(oldObj.(*coreV1.Node).Annotations, node.Annotations)
					change, _ := json.Marshal(diffs)
					log.Debug().Str("change", string(change)).Msg("change")
				}
				updateNode(node)
			},
		},
	)

	// Now let's start the controller
	stop := make(chan struct{})
	defer close(stop)
	go controller.Run(stop)

	// Wait forever
	select {}
}

func updateNode(node *coreV1.Node) {
	publicIP := ""
	// The externalIP status information is typically only available if a cloud provider
	// controller is enabled in the cluster, e.g. Openstack Cloud Provider
	for _, nodeAddress := range node.Status.Addresses {
		if nodeAddress.Type == coreV1.NodeExternalIP {
			publicIP = nodeAddress.Address
			break
		}
	}
	logEntry := log.Info().Str("node_name", node.Name)

	if publicIP == "" {
		//fallback on RKE annotation
		var ok bool
		publicIP, ok = node.Annotations[rkeExternalIPAnnotation]
		if !ok {
			logEntry.Msg(fmt.Sprintf("node doesn't have public address or %s a annotation, skipping", rkeExternalIPAnnotation))
			return
		}
	}
	logEntry.Str("public_ip", publicIP)

	flannelPublicIP := getValueFromMap(flannelPublicIPAnnotation, node.Annotations)
	flannelPublicIPOverride := getValueFromMap(flannelPublicIPOverrideAnnotation, node.Annotations)
	logEntry.Str("public_ip", publicIP).
		Str("old_flannel_public_ip", flannelPublicIP).
		Str("old_flannel_public_ip_override", flannelPublicIPOverride)
	if publicIP == flannelPublicIP && publicIP == flannelPublicIPOverride {
		//all good, nothing to see here.
		return
	}

	//set new values
	node.Annotations[flannelPublicIPAnnotation] = publicIP
	node.Annotations[flannelPublicIPOverrideAnnotation] = publicIP
	logEntry.Str("new_flannel_public_ip", publicIP).
		Str("new_flannel_public_ip_override", publicIP)

	_, err := clientset.CoreV1().Nodes().Update(node)
	if err != nil {
		log.Fatal().Err(err).Str("node_name", node.Name).Msg("cannot update node annotation")
	}
	logEntry.Msg("updated node annotation")
}

// getValueFromMap returns value from map or an empty string in case it's not found
func getValueFromMap(key string, obj map[string]string) string {
	if val, ok := obj[key]; ok {
		return val
	}
	return ""
}
