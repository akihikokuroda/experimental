package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/cloudevents/sdk-go/pkg/cloudevents"
	"github.com/cloudevents/sdk-go/pkg/cloudevents/client"
	"github.com/cloudevents/sdk-go/pkg/cloudevents/transport/http"
	"github.com/joeshaw/envdecode"
	experimentalClientset "github.com/tektoncd/experimental/tekton-listener/pkg/client/clientset/versioned"

	pipelinev1alpha1 "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1alpha1"
	pipelineClientset "github.com/tektoncd/pipeline/pkg/client/clientset/versioned"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/knative/pkg/logging"
	"github.com/pkg/errors"
	gh "gopkg.in/go-playground/webhooks.v5/github"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	listenerPath   = "/events"
	cloudEventType = "cloudevent"
)

type Config struct {
	Event            string `env:"EVENT,default=cloudevent"`
	EventType        string `env:"EVENT_TYPE,default=com.github.checksuite"`
	MasterURL        string `env:"MASTER_URL"`
	Kubeconfig       string `env:"KUBECONFIG"`
	Namespace        string `env:"NAMESPACE"`
	ServiceAccount   string `env:"SERVICEACCOUNT"`
	ListenerResource string `env:"LISTENER_RESOURCE"`
	Port             int    `env:"PORT,default=8082"`
	SetBuildSha      bool   `env:"SETBUILDSHA"`
}

// EventListener starts an event receiver to accept data to trigger pipelineruns.
type EventListener struct {
	event               string
	eventType           string
	namespace           string
	runName             string
	serviceAccount      string
	pipelineClientset   pipelineClientset.Interface
	experimentClientset experimentalClientset.Interface
	mux                 *sync.Mutex
	runSpec             pipelinev1alpha1.PipelineRunSpec
	port                int
	setBuildSha         bool
}

func main() {
	var cfg Config
	err := envdecode.Decode(&cfg)
	if err != nil {
		log.Fatalf("Failed loading env config: %q", err)
	}

	logger, _ := logging.NewLogger("", "event-listener")
	defer logger.Sync()

	if cfg.Namespace == "" {
		log.Fatal("NAMESPACE env var can not be empty")
	}

	clientcfg, err := clientcmd.BuildConfigFromFlags(cfg.MasterURL, cfg.Kubeconfig)
	if err != nil {
		logger.Fatalf("Error building kubeconfig: %v", err)
	}

	pipelineClient, err := pipelineClientset.NewForConfig(clientcfg)
	if err != nil {
		logger.Fatalf("Error building pipeline clientset: %v", err)
	}
	experimentClient, err := experimentalClientset.NewForConfig(clientcfg)
	if err != nil {
		logger.Fatalf("Error building experimental tekton clientset: %v", err)
	}

	listener, err := experimentClient.PipelineexperimentalV1alpha1().TektonListeners(cfg.Namespace).Get(cfg.ListenerResource, metav1.GetOptions{})
	if err != nil {
		log.Fatalf("failed to get tekton listener spec: %s in namespace: %s error: %q", cfg.ListenerResource, cfg.Namespace, err)
	}
	listenerName := fmt.Sprintf("%s-%d", listener.Name, cfg.Port)
	e := &EventListener{
		event:               cfg.Event,
		eventType:           cfg.EventType,
		port:                cfg.Port,
		namespace:           cfg.Namespace,
		mux:                 &sync.Mutex{},
		pipelineClientset:   pipelineClient,
		experimentClientset: experimentClient,
		runName:             listenerName,
		runSpec:             *listener.Spec.PipelineRunSpec,
		setBuildSha:         cfg.SetBuildSha,
		serviceAccount:      cfg.ServiceAccount,
	}

	switch e.event {
	case cloudEventType:
		e.startCloudEventListener() // handle cloud events
	default:
		log.Fatalf("invalid event type: %q", e.event)
	}
}

func (e *EventListener) startCloudEventListener() {
	log.Printf("Starting listener on port %d", e.port)

	t, err := http.New(
		http.WithPort(e.port),
		http.WithPath(listenerPath),
	)
	if err != nil {
		log.Fatalf("failed to create http client, %v", err)
	}
	client, err := client.New(t, client.WithTimeNow(), client.WithUUIDs())
	if err != nil {
		log.Fatalf("failed to create client, %v", err)
	}

	log.Fatalf("Failed to start cloudevent receiver: %q", client.StartReceiver(context.Background(), e.HandleRequest))
}

// HandleRequest will decode the body of the cloudevent into the correct payload type based on event type,
// match on the event type and submit build from repo/branch.
// Only check_suite events are supported.
func (e *EventListener) HandleRequest(ctx context.Context, event cloudevents.Event) error {
	// todo: contribute nil check upstream
	if event.Context == nil {
		return errors.New("Empty event context")
	}

	if event.SpecVersion() != "0.2" {
		return errors.New("Only cloudevents version 0.2 supported")
	}
	if event.Type() != e.eventType {
		return errors.New("Mismatched event type submitted")

	}

	log.Printf("Handling event Type: %q", event.Type())

	switch event.Type() {
	case "com.github.checksuite":
		cs := &gh.CheckSuitePayload{}
		if err := event.DataAs(cs); err != nil {
			return errors.Wrap(err, "Error handling check suite payload")
		}
		if err := e.handleCheckSuite(event, cs); err != nil {
			return err
		}
	}

	return nil
}

func (r *EventListener) handleCheckSuite(event cloudevents.Event, cs *gh.CheckSuitePayload) error {
	if cs.CheckSuite.Conclusion == "success" {
		build, err := r.createPipelineRun(cs.CheckSuite.HeadSHA)
		if err != nil {
			return errors.Wrapf(err, "Error creating pipeline run for check_suite event: %q", event.Type())
		}

		log.Printf("Created pipeline run %q!", build.Name)
	}
	return nil
}

func (e *EventListener) createPipelineRun(sha string) (*pipelinev1alpha1.PipelineRun, error) {
	e.mux.Lock()
	defer e.mux.Unlock()

	pr := &pipelinev1alpha1.PipelineRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      e.runName,
			Namespace: e.namespace,
		},
	}
	// copy the spec template into place
	pr.Spec = e.runSpec

	if e.setBuildSha {
		// if enabled, set the builds git revision to the github events SHA
		for _, param := range pr.Spec.Params {
			switch {
			case strings.EqualFold(param.Name, "Revision"):
				param.Value = sha
			default:
				log.Print("No SHA param to update")
			}
		}
	}

	log.Printf("Creating pipelinerun %q sha %q namespace %q", pr.Name, sha, pr.Namespace)

	run, err := e.pipelineClientset.Tekton().PipelineRuns(e.namespace).Create(pr)
	if err != nil {
		log.Fatalf("failed to get pipeline listener spec: %q", err)
	}

	return run, nil
}
