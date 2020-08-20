package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"

	"encoding/json"

	flag "github.com/spf13/pflag"
	"k8s.io/api/core/v1"
	k8sErr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type replicateRule struct {
	OriginNamespace  string   `json:"origNs"`
	OriginName       string   `json:"origName"`
	TargetNamespaces []string `json:"targetNs"`
}

var rules []replicateRule

var watchers []watch.Interface
var outChan chan watch.Event

var syncerNs *string
var k8sRuleCm *string
var ruleFile *string

func main() {
	var config *rest.Config
	var err error
	var local = flag.Bool("local", false, "Specify whether to use local kubeconfig or generate one from within the cluster")
	syncerNs = flag.String("cmns", "secret-syncer", "Specify the namespace in which secret-syncers configmap resides")
	k8sRuleCm = flag.String("cmname", "secret-syncer", "Specify the name of secret-syncers configmap")
	ruleFile = flag.String("rulesfile", "rules.json", "Specify the name of the rules file inside the configmap")
	flag.Parse()

	if *local == true {
		// Use the local kubeconfig file
		kubeconfig := filepath.Join(os.Getenv("HOME"), ".kube", "config")
		fmt.Println("Using kubeconfig: ", kubeconfig)
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		// Generate a kubeconfig from within the cluster
		fmt.Println("Using in-cluster config")
		config, err = rest.InClusterConfig()
	}

	if err != nil {
		panic(err.Error())
	}

	// create the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatal(err)
	}

	// watch for changes in the rules configmap
	log.Printf("Searching for configmap %s/%s' now", *syncerNs, *k8sRuleCm)
	fieldSelector := fmt.Sprintf("metadata.name==%s", *k8sRuleCm)
	rulesWatcher, err := clientset.CoreV1().ConfigMaps(*syncerNs).Watch(metav1.ListOptions{FieldSelector: fieldSelector})
	if err != nil {
		log.Fatalf("Could not watch configmap'%s/%s': %v", *syncerNs, *k8sRuleCm, err)
	}

	confCh := rulesWatcher.ResultChan()

	for event := range confCh {
		cm, ok := event.Object.(*v1.ConfigMap)

		if !ok {
			log.Fatal("Could not fetch cm object due to unexpected type")
		}

		switch event.Type {
		case watch.Added:
			log.Printf("Found existing config. Establishing watches now.")
			rules = parseConfig(cm)
			establishWatchers(clientset, rules)
		case watch.Modified:
			log.Printf("Config has changed. Updating watches now")
			rules = parseConfig(cm)
			establishWatchers(clientset, rules)
		}

	}
}

func establishWatchers(clientset *kubernetes.Clientset, rules []replicateRule) {
	// Stop any existing watchers
	log.Printf("Stopping any existing watches")
	if outChan != nil {
		close(outChan)
	}
	var wg sync.WaitGroup
	wg.Add(len(watchers))
	for _, watcher := range watchers {
		go func(watcher watch.Interface) {
			watcher.Stop()
			wg.Done()
		}(watcher)
	}
	wg.Wait()
	log.Printf("Successfully stopped all old watches")

	// Prepare watchers and add them to slice
	log.Printf("Preparing new watches now")
	for _, rule := range rules {
		fieldSelector := fmt.Sprintf("metadata.name==%s", rule.OriginName)
		watcher, err := clientset.CoreV1().Secrets(rule.OriginNamespace).Watch(metav1.ListOptions{FieldSelector: fieldSelector})
		if err != nil {
			log.Printf("Could not establish watch on %s/%s: '%v'\n", rule.OriginNamespace, rule.OriginName, err)
			continue
		}
		log.Printf("Successfully established watch on %s/%s\n", rule.OriginNamespace, rule.OriginName)
		watchers = append(watchers, watcher)
	}

	// Merge all watchers
	outChan := mergeWatcherChans(watchers)

	go func(outChan chan watch.Event) {
		for event := range outChan {
			secret, ok := event.Object.(*v1.Secret)
			if !ok {
				log.Fatal("Could not fetch secret object due to unexpected type")
			}

			switch event.Type {

			case watch.Added:
				log.Printf("Secret '%s/%s' was added. Triggering Replication\n", secret.Namespace, secret.Name)
				replicateSecret(clientset, secret)

			case watch.Modified:
				log.Printf("Secret '%s/%s' was modified\n", secret.Namespace, secret.Name)
				replicateSecret(clientset, secret)
			}
		}
	}(outChan)
}

func parseConfig(cm *v1.ConfigMap) []replicateRule {
	file := cm.Data[*ruleFile]

	if file == "" {
		log.Fatalf("Could not find file '%s' in configmap '%s/%s'", *ruleFile, *syncerNs, *k8sRuleCm)
	}

	rules := []replicateRule{}

	err := json.Unmarshal([]byte(file), &rules)

	if err != nil {
		log.Fatalf("Could not parse '%s': '%v'", *ruleFile, err)
	}
	// Make sure rules were actually parsed
	if rules[0].OriginNamespace == "" {
		log.Fatalf("Could not find a valid rule")
	}
	return rules
}

// Merge the separate channels of the different watchers into one channel
func mergeWatcherChans(watchers []watch.Interface) chan watch.Event {
	out := make(chan watch.Event)
	go func() {
		var wg sync.WaitGroup
		wg.Add(len(watchers))

		for _, watcher := range watchers {
			c := watcher.ResultChan()
			go func(c <-chan watch.Event) {
				for v := range c {
					out <- v
				}
				wg.Done()
			}(c)
		}
		wg.Wait()
		close(out)
	}()
	return out
}

func replicateSecret(cs *kubernetes.Clientset, secret *v1.Secret) {
	// Find the rule corresponding to the secret
	rule := ruleFromSecret(secret)

	// Comparing for name here instead of the whole struct, because structs containing []string cannot be compared
	if rule.OriginName == "" {
		log.Printf("Could not find a rule for secret '%s/%s'. Aborting replication. Please have a look in rules.yaml", secret.Namespace, secret.Name)
		return
	}
	log.Printf("Found rule for secret '%s/%s': %v\n", secret.Namespace, secret.Name, rule)
	// Start Replication
	for _, ns := range rule.TargetNamespaces {
		go func(cs *kubernetes.Clientset, ns string, secret *v1.Secret) {
			// Make a copy of original secret and change its namespace
			secretPayload := *secret
			secretPayload.SetNamespace(ns)
			secretPayload.SetResourceVersion("")
			secretPayload.SetUID("")
			secretPayload.SetCreationTimestamp(metav1.Time{})

			// Check if the secret already exists
			_, err := cs.CoreV1().Secrets(ns).Get(secret.Name, metav1.GetOptions{})

			switch {

			// Secret was found => update existing secret
			case err == nil:
				res, err := cs.CoreV1().Secrets(ns).Update(&secretPayload)
				if err != nil {
					log.Printf("Could not update secret '%s/%s': %v\n", secret.Namespace, secret.Name, err)
					return
				}
				log.Printf("Successfully updated secret '%s/%s'\n", res.Namespace, res.Name)
				return

			// An error occurred => we need to check if it was just a 404 in which case we will just create the secret 
			// or if it was an unexpected error
			case err != nil:
				// Cast error so we can get its statuscode
				fmtErr, ok := (err).(*k8sErr.StatusError)
				if !ok {
					log.Fatal("Could not fetch http error code due to unexpected type")
				}

				// Secret was not found => create a new secret
				if fmtErr.ErrStatus.Code == 404 {
					res, err := cs.CoreV1().Secrets(ns).Create(&secretPayload)
					if err != nil {
						log.Printf("Could not create secret '%s/%s': %v\n", secretPayload.Namespace, secretPayload.Name, err)
						return
					}
					log.Printf("Successfully created secret '%s/%s'\n", res.Namespace, res.Name)
					return
				}

				// An unexpected error occurred
				log.Printf("Could not check status of secret '%s/%s': %v\n", secret.Namespace, secret.Name, err)
				return

			}
		}(cs, ns, secret)
	}
}

// This function is needed because the watchers will only return
// the secret that has changed, but we also need to know the
// correspoding rule
func ruleFromSecret(secret *v1.Secret) replicateRule {
	for _, rule := range rules {
		if rule.OriginNamespace == secret.Namespace && rule.OriginName == secret.Name {
			return rule
		}
	}
	return replicateRule{}
}
