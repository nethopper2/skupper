package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"path"
	"strconv"
	"syscall"
	"time"

	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	clientpodman "github.com/skupperproject/skupper/client/podman"
	"github.com/skupperproject/skupper/pkg/config"
	"github.com/skupperproject/skupper/pkg/domain/podman"
	"github.com/skupperproject/skupper/pkg/utils"

	"github.com/skupperproject/skupper/api/types"
	"github.com/skupperproject/skupper/client"
	"github.com/skupperproject/skupper/pkg/certs"
	"github.com/skupperproject/skupper/pkg/kube"
	"github.com/skupperproject/skupper/pkg/version"
)

// should this be in utils?
type tlsConfig struct {
	Ca     string `json:"ca,omitempty"`
	Cert   string `json:"cert,omitempty"`
	Key    string `json:"key,omitempty"`
	Verify bool   `json:"recType,omitempty"`
}

type connectJson struct {
	Scheme string    `json:"scheme,omitempty"`
	Host   string    `json:"host,omitempty"`
	Port   string    `json:"port,omitempty"`
	Tls    tlsConfig `json:"tls,omitempty"`
}

type UserResponse struct {
	Username string `json:"username"`
	AuthMode string `json:"authType"`
}

var onlyOneSignalHandler = make(chan struct{})
var shutdownSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

func getConnectInfo(file string) (connectJson, error) {
	cj := connectJson{}

	jsonFile, err := os.ReadFile(file)
	if err != nil {
		return cj, err
	}

	err = json.Unmarshal(jsonFile, &cj)
	if err != nil {
		return cj, err
	}

	return cj, nil
}

func SetupSignalHandler() (stopCh <-chan struct{}) {
	close(onlyOneSignalHandler) // panics when called twice

	stop := make(chan struct{})
	c := make(chan os.Signal, 2)
	signal.Notify(c, shutdownSignals...)
	go func() {
		<-c
		close(stop)
		<-c
		os.Exit(1) // second signal. Exit directly.
	}()

	return stop
}

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			origin = "*"
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,DELETE")
		next.ServeHTTP(w, r)
	})
}

func authenticate(dir string, user string, password string) bool {
	filename := path.Join(dir, user)
	file, err := os.Open(filename)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			log.Printf("COLLECTOR: Failed to authenticate %s, no such user exists", user)
		} else {
			log.Printf("COLLECTOR: Failed to authenticate %s: %s", user, err)
		}
		return false
	}
	defer file.Close()

	bytes, err := io.ReadAll(file)
	if err != nil {
		log.Printf("COLLECTOR: Failed to authenticate %s: %s", user, err)
		return false
	}
	return string(bytes) == password
}

func authenticated(h http.HandlerFunc) http.HandlerFunc {
	dir := os.Getenv("FLOW_USERS")

	if dir != "" {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, password, ok := r.BasicAuth()

			if ok && authenticate(dir, user, password) {
				h.ServeHTTP(w, r)
			} else {
				w.Header().Set("WWW-Authenticate", "Basic realm=skupper")
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
			}
		})
	} else {
		return h
	}
}

func getOpenshiftUser(r *http.Request) UserResponse {
	userResponse := UserResponse{
		Username: "",
		AuthMode: string(types.ConsoleAuthModeOpenshift),
	}

	if cookie, err := r.Cookie("_oauth_proxy"); err == nil && cookie != nil {
		if cookieDecoded, _ := base64.StdEncoding.DecodeString(cookie.Value); cookieDecoded != nil {
			userResponse.Username = string(cookieDecoded)
		}
	}

	return userResponse
}

func getInternalUser(r *http.Request) UserResponse {
	userResponse := UserResponse{
		Username: "",
		AuthMode: string(types.ConsoleAuthModeInternal),
	}

	user, _, ok := r.BasicAuth()

	if ok {
		userResponse.Username = user
	}

	return userResponse
}

func getUnsecuredUser(r *http.Request) UserResponse {
	return UserResponse{
		Username: "",
		AuthMode: string(types.ConsoleAuthModeUnsecured)}
}

func openshiftLogout(w http.ResponseWriter, r *http.Request) {
	// Create a new cookie with MaxAge set to -1 to delete the existing cookie.
	cookie := http.Cookie{
		Name:   "_oauth_proxy", // openshift cookie name
		Path:   "/",
		MaxAge: -1,
		Domain: r.Host,
	}

	http.SetCookie(w, &cookie)
}

func internalLogout(w http.ResponseWriter, r *http.Request, validNonces map[string]bool) {
	queryParams := r.URL.Query()
	nonce := queryParams.Get("nonce")

	// When I logout, the browser open the prompt again and , if the credentials are correct, it calls the logout again.
	// We track the second call using the nonce set from the client app to avoid loop of unauthenticated calls.
	if _, exists := validNonces[nonce]; exists {
		delete(validNonces, nonce)
		fmt.Fprintf(w, "%s", "Logged out")

		return
	}

	validNonces[nonce] = true
	w.Header().Set("WWW-Authenticate", "Basic realm=skupper")
	http.Error(w, "Unauthorized", http.StatusUnauthorized)
}

func main() {
	flags := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	// if -version used, report and exit
	isVersion := flags.Bool("version", false, "Report the version of the Skupper Flow Collector")
	isProf := flags.Bool("profile", false, "Exposes the runtime profiling facilities from net/http/pprof on http://localhost:9970")

	flags.Parse(os.Args[1:])
	if *isVersion {
		fmt.Println(version.Version)
		os.Exit(0)
	}

	// Startup message
	log.Printf("COLLECTOR: Starting Skupper Flow collector controller version %s \n", version.Version)

	origin := os.Getenv("SKUPPER_SITE_ID")
	namespace := os.Getenv("SKUPPER_NAMESPACE")

	// set up signals so we handle the first shutdown signal gracefully
	stopCh := SetupSignalHandler()

	platform := config.GetPlatform()
	var flowRecordTtl time.Duration
	var enableConsole bool
	var prometheusUrl string
	var authMode string
	//collecting valid nonces for internal auth mode
	var validNonces = make(map[string]bool)

	// waiting on skupper-router to be available
	if platform == "" || platform == types.PlatformKubernetes {
		cli, err := client.NewClient(namespace, "", "")
		if err != nil {
			log.Fatal("COLLECTOR: Error getting van client", err.Error())
		}

		log.Println("COLLECTOR: Waiting for Skupper router component to start")
		_, err = kube.WaitDeploymentReady(types.TransportDeploymentName, namespace, cli.KubeClient, time.Second*180, time.Second*5)
		if err != nil {
			log.Fatal("COLLECTOR: Error waiting for transport deployment to be ready ", err.Error())
		}

		siteConfig, err := cli.SiteConfigInspect(context.Background(), nil)
		if err != nil {
			log.Fatal("COLLECTOR: Error getting site config", err.Error())
		}

		flowRecordTtl = siteConfig.Spec.FlowCollector.FlowRecordTtl
		enableConsole = siteConfig.Spec.EnableConsole
		authMode = siteConfig.Spec.AuthMode

		svc, err := kube.GetService(types.PrometheusServiceName, cli.Namespace, cli.KubeClient)
		if err == nil {
			prometheusUrl = "http://" + svc.Spec.ClusterIP + ":" + fmt.Sprint(svc.Spec.Ports[0].Port) + "/api/v1/"
		}
	} else {
		cfg, err := podman.NewPodmanConfigFileHandler().GetConfig()
		if err != nil {
			log.Fatal("Error reading podman site config", err)
		}
		podmanCli, err := clientpodman.NewPodmanClient(cfg.Endpoint, "")
		if err != nil {
			log.Fatal("Error creating podman client", err)
		}
		err = utils.Retry(time.Second, 120, func() (bool, error) {
			router, err := podmanCli.ContainerInspect(types.TransportDeploymentName)
			if err != nil {
				return false, fmt.Errorf("error retrieving %s container state - %w", types.TransportDeploymentName, err)
			}
			if !router.Running {
				return false, nil
			}
			return true, nil
		})
		if err != nil {
			log.Fatalf("unable to determine if %s container is running - %s", types.TransportDeploymentName, err)
		}
		flowRecordTtl, _ = time.ParseDuration(os.Getenv("FLOW_RECORD_TTL"))
		enableConsole, _ = strconv.ParseBool(os.Getenv("ENABLE_CONSOLE"))
		prometheusUrl = "http://skupper-prometheus:9090/api/v1/"

		flowUsers := os.Getenv("FLOW_USERS")
		// Podman support only unsecured auth mode and internal auth mode
		authMode = types.ConsoleAuthModeUnsecured
		if flowUsers != "" {
			authMode = types.ConsoleAuthModeInternal
		}
	}

	tlsConfig := certs.GetTlsConfigRetriever(true, types.ControllerConfigPath+"tls.crt", types.ControllerConfigPath+"tls.key", types.ControllerConfigPath+"ca.crt")

	conn, err := getConnectInfo(types.ControllerConfigPath + "connect.json")
	if err != nil {
		log.Fatal("Error getting connect.json", err.Error())
	}

	reg := prometheus.NewRegistry()
	c, err := NewController(origin, reg, conn.Scheme, conn.Host, conn.Port, tlsConfig, flowRecordTtl)
	if err != nil {
		log.Fatal("Error getting new flow collector ", err.Error())
	}
	c.FlowCollector.Collector.PrometheusUrl = prometheusUrl

	// map the authentication mode with the function to get the user
	userMap := make(map[string]func(*http.Request) UserResponse)
	userMap[string(types.ConsoleAuthModeOpenshift)] = getOpenshiftUser
	userMap[string(types.ConsoleAuthModeInternal)] = getInternalUser
	userMap[string(types.ConsoleAuthModeUnsecured)] = getUnsecuredUser

	logoutMap := make(map[string]func(http.ResponseWriter, *http.Request))
	logoutMap[string(types.ConsoleAuthModeOpenshift)] = openshiftLogout
	logoutMap[string(types.ConsoleAuthModeInternal)] = func(w http.ResponseWriter, r *http.Request) {
		internalLogout(w, r, validNonces)
	}

	var mux = mux.NewRouter().StrictSlash(true)

	var api = mux.PathPrefix("/api").Subrouter()
	api.NotFoundHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	if os.Getenv("USE_CORS") != "" {
		api.Use(cors)
	}

	var api1 = api.PathPrefix("/v1alpha1").Subrouter()
	api1.NotFoundHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	var logUri = os.Getenv("LOG_REQ_URI")
	if logUri == "true" {
		api1.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				log.Printf("COLLECTOR: request uri %s \n", r.RequestURI)
				next.ServeHTTP(w, r)
			})
		})
	}

	var api1Internal = api1.PathPrefix("/internal").Subrouter()
	api1Internal.NotFoundHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	var promApi = api1Internal.PathPrefix("/prom").Subrouter()
	promApi.StrictSlash(true)
	promApi.NotFoundHandler = authenticated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	var promqueryApi = promApi.PathPrefix("/query").Subrouter()
	promqueryApi.StrictSlash(true)
	promqueryApi.HandleFunc("/", authenticated(http.HandlerFunc(c.promqueryHandler)))
	promqueryApi.NotFoundHandler = authenticated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	var promqueryrangeApi = promApi.PathPrefix("/rangequery").Subrouter()
	promqueryrangeApi.StrictSlash(true)
	promqueryrangeApi.HandleFunc("/", authenticated(http.HandlerFunc(c.promqueryrangeHandler)))
	promqueryrangeApi.NotFoundHandler = authenticated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	var metricsApi = api1.PathPrefix("/metrics").Subrouter()
	metricsApi.StrictSlash(true)
	metricsApi.Handle("/", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))

	var eventsourceApi = api1.PathPrefix("/eventsources").Subrouter()
	eventsourceApi.StrictSlash(true)
	eventsourceApi.HandleFunc("/", http.HandlerFunc(c.eventsourceHandler)).Name("list")
	eventsourceApi.HandleFunc("/{id}", http.HandlerFunc(c.eventsourceHandler)).Name("item")
	eventsourceApi.NotFoundHandler = authenticated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	var userApi = api1.PathPrefix("/user").Subrouter()
	userApi.StrictSlash(true)
	userApi.HandleFunc("/", authenticated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler, exists := userMap[authMode]

		if !exists {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		response, err := json.Marshal(handler(r))

		if err != nil {
			log.Printf("Error /user response: %s", err.Error())
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write(response)
	})))

	var userLogout = api1.PathPrefix("/logout").Subrouter()
	userLogout.StrictSlash(true)
	userLogout.HandleFunc("/", authenticated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler, exists := logoutMap[authMode]
		if exists {
			handler(w, r)
		}
	})))

	var siteApi = api1.PathPrefix("/sites").Subrouter()
	siteApi.StrictSlash(true)
	siteApi.HandleFunc("/", authenticated(http.HandlerFunc(c.siteHandler))).Name("list")
	siteApi.HandleFunc("/{id}", authenticated(http.HandlerFunc(c.siteHandler))).Name("item")
	siteApi.HandleFunc("/{id}/processes", authenticated(http.HandlerFunc(c.siteHandler))).Name("processes")
	siteApi.HandleFunc("/{id}/routers", authenticated(http.HandlerFunc(c.siteHandler))).Name("routers")
	siteApi.HandleFunc("/{id}/links", authenticated(http.HandlerFunc(c.siteHandler))).Name("links")
	siteApi.HandleFunc("/{id}/hosts", authenticated(http.HandlerFunc(c.siteHandler))).Name("hosts")
	siteApi.NotFoundHandler = authenticated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	var hostApi = api1.PathPrefix("/hosts").Subrouter()
	hostApi.StrictSlash(true)
	hostApi.HandleFunc("/", authenticated(http.HandlerFunc(c.hostHandler))).Name("list")
	hostApi.HandleFunc("/{id}", authenticated(http.HandlerFunc(c.hostHandler))).Name("item")
	hostApi.NotFoundHandler = authenticated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	var routerApi = api1.PathPrefix("/routers").Subrouter()
	routerApi.StrictSlash(true)
	routerApi.HandleFunc("/", authenticated(http.HandlerFunc(c.routerHandler))).Name("list")
	routerApi.HandleFunc("/{id}", authenticated(http.HandlerFunc(c.routerHandler))).Name("item")
	routerApi.HandleFunc("/{id}/flows", authenticated(http.HandlerFunc(c.routerHandler))).Name("flows")
	routerApi.HandleFunc("/{id}/links", authenticated(http.HandlerFunc(c.routerHandler))).Name("links")
	routerApi.HandleFunc("/{id}/listeners", authenticated(http.HandlerFunc(c.routerHandler))).Name("listeners")
	routerApi.HandleFunc("/{id}/connectors", authenticated(http.HandlerFunc(c.routerHandler))).Name("connectors")
	routerApi.NotFoundHandler = authenticated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	var linkApi = api1.PathPrefix("/links").Subrouter()
	linkApi.StrictSlash(true)
	linkApi.HandleFunc("/", authenticated(http.HandlerFunc(c.linkHandler))).Name("list")
	linkApi.HandleFunc("/{id}", authenticated(http.HandlerFunc(c.linkHandler))).Name("item")
	linkApi.NotFoundHandler = authenticated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	var listenerApi = api1.PathPrefix("/listeners").Subrouter()
	listenerApi.StrictSlash(true)
	listenerApi.HandleFunc("/", authenticated(http.HandlerFunc(c.listenerHandler))).Name("list")
	listenerApi.HandleFunc("/{id}", authenticated(http.HandlerFunc(c.listenerHandler))).Name("item")
	listenerApi.HandleFunc("/{id}/flows", authenticated(http.HandlerFunc(c.listenerHandler))).Name("flows")
	listenerApi.NotFoundHandler = authenticated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	var connectorApi = api1.PathPrefix("/connectors").Subrouter()
	connectorApi.StrictSlash(true)
	connectorApi.HandleFunc("/", authenticated(http.HandlerFunc(c.connectorHandler))).Name("list")
	connectorApi.HandleFunc("/{id}", authenticated(http.HandlerFunc(c.connectorHandler))).Name("item")
	connectorApi.HandleFunc("/{id}/flows", authenticated(http.HandlerFunc(c.connectorHandler))).Name("flows")
	connectorApi.HandleFunc("/{id}/process", authenticated(http.HandlerFunc(c.connectorHandler))).Name("process")
	connectorApi.NotFoundHandler = authenticated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	var addressApi = api1.PathPrefix("/addresses").Subrouter()
	addressApi.StrictSlash(true)
	addressApi.HandleFunc("/", authenticated(http.HandlerFunc(c.addressHandler))).Name("list")
	addressApi.HandleFunc("/{id}", authenticated(http.HandlerFunc(c.addressHandler))).Name("item")
	addressApi.HandleFunc("/{id}/processes", authenticated(http.HandlerFunc(c.addressHandler))).Name("processes")
	addressApi.HandleFunc("/{id}/processpairs", authenticated(http.HandlerFunc(c.addressHandler))).Name("processpairs")
	addressApi.HandleFunc("/{id}/flows", authenticated(http.HandlerFunc(c.addressHandler))).Name("flows")
	addressApi.HandleFunc("/{id}/flowpairs", authenticated(http.HandlerFunc(c.addressHandler))).Name("flowpairs")
	addressApi.HandleFunc("/{id}/listeners", authenticated(http.HandlerFunc(c.addressHandler))).Name("listeners")
	addressApi.HandleFunc("/{id}/connectors", authenticated(http.HandlerFunc(c.addressHandler))).Name("connectors")
	addressApi.NotFoundHandler = authenticated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	var processApi = api1.PathPrefix("/processes").Subrouter()
	processApi.StrictSlash(true)
	processApi.HandleFunc("/", authenticated(http.HandlerFunc(c.processHandler))).Name("list")
	processApi.HandleFunc("/{id}", authenticated(http.HandlerFunc(c.processHandler))).Name("item")
	processApi.HandleFunc("/{id}/flows", authenticated(http.HandlerFunc(c.processHandler))).Name("flows")
	processApi.HandleFunc("/{id}/addresses", authenticated(http.HandlerFunc(c.processHandler))).Name("addresses")
	processApi.HandleFunc("/{id}/connector", authenticated(http.HandlerFunc(c.processHandler))).Name("connector")
	processApi.NotFoundHandler = authenticated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	var processGroupApi = api1.PathPrefix("/processgroups").Subrouter()
	processGroupApi.StrictSlash(true)
	processGroupApi.HandleFunc("/", authenticated(http.HandlerFunc(c.processGroupHandler))).Name("list")
	processGroupApi.HandleFunc("/{id}", authenticated(http.HandlerFunc(c.processGroupHandler))).Name("item")
	processGroupApi.HandleFunc("/{id}/processes", authenticated(http.HandlerFunc(c.processGroupHandler))).Name("processes")
	processGroupApi.NotFoundHandler = authenticated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	var flowApi = api1.PathPrefix("/flows").Subrouter()
	flowApi.StrictSlash(true)
	flowApi.HandleFunc("/", authenticated(http.HandlerFunc(c.flowHandler))).Name("list")
	flowApi.HandleFunc("/{id}", authenticated(http.HandlerFunc(c.flowHandler))).Name("item")
	flowApi.HandleFunc("/{id}/process", authenticated(http.HandlerFunc(c.flowHandler))).Name("process")
	flowApi.NotFoundHandler = authenticated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	var flowpairApi = api1.PathPrefix("/flowpairs").Subrouter()
	flowpairApi.StrictSlash(true)
	flowpairApi.HandleFunc("/", authenticated(http.HandlerFunc(c.flowPairHandler))).Name("list")
	flowpairApi.HandleFunc("/{id}", authenticated(http.HandlerFunc(c.flowPairHandler))).Name("item")
	flowpairApi.NotFoundHandler = authenticated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	var sitepairApi = api1.PathPrefix("/sitepairs").Subrouter()
	sitepairApi.StrictSlash(true)
	sitepairApi.HandleFunc("/", authenticated(http.HandlerFunc(c.sitePairHandler))).Name("list")
	sitepairApi.HandleFunc("/{id}", authenticated(http.HandlerFunc(c.sitePairHandler))).Name("item")
	sitepairApi.NotFoundHandler = authenticated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	var processgrouppairApi = api1.PathPrefix("/processgrouppairs").Subrouter()
	processgrouppairApi.StrictSlash(true)
	processgrouppairApi.HandleFunc("/", authenticated(http.HandlerFunc(c.processGroupPairHandler))).Name("list")
	processgrouppairApi.HandleFunc("/{id}", authenticated(http.HandlerFunc(c.processGroupPairHandler))).Name("item")
	processgrouppairApi.NotFoundHandler = authenticated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	var processpairApi = api1.PathPrefix("/processpairs").Subrouter()
	processpairApi.StrictSlash(true)
	processpairApi.HandleFunc("/", authenticated(http.HandlerFunc(c.processPairHandler))).Name("list")
	processpairApi.HandleFunc("/{id}", authenticated(http.HandlerFunc(c.processPairHandler))).Name("item")
	processpairApi.NotFoundHandler = authenticated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	if enableConsole {
		mux.PathPrefix("/").Handler(http.FileServer(http.Dir("/app/console/")))
	} else {
		log.Println("COLLECTOR: Skupper console is disabled")
	}

	var collectorApi = api1.PathPrefix("/collectors").Subrouter()
	collectorApi.StrictSlash(true)
	collectorApi.HandleFunc("/", authenticated(http.HandlerFunc(c.collectorHandler))).Name("list")
	collectorApi.HandleFunc("/{id}", authenticated(http.HandlerFunc(c.collectorHandler))).Name("item")
	collectorApi.HandleFunc("/{id}/connectors-to-process", authenticated(http.HandlerFunc(c.collectorHandler))).Name("connectors-to-process")
	collectorApi.HandleFunc("/{id}/flows-to-pair", authenticated(http.HandlerFunc(c.collectorHandler))).Name("flows-to-pair")
	collectorApi.HandleFunc("/{id}/flows-to-process", authenticated(http.HandlerFunc(c.collectorHandler))).Name("flows-to-process")
	collectorApi.HandleFunc("/{id}/pair-to-aggregate", authenticated(http.HandlerFunc(c.collectorHandler))).Name("pair-to-aggregate")
	collectorApi.NotFoundHandler = authenticated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	addr := ":8010"
	if os.Getenv("FLOW_PORT") != "" {
		addr = ":" + os.Getenv("FLOW_PORT")
	}
	if os.Getenv("FLOW_HOST") != "" {
		addr = os.Getenv("FLOW_HOST") + addr
	}
	log.Printf("COLLECTOR: server listening on %s", addr)
	s := &http.Server{
		Addr:    addr,
		Handler: handlers.CompressHandler(mux),
	}

	go func() {
		_, err := os.Stat("/etc/service-controller/console/tls.crt")
		if err == nil {
			err := s.ListenAndServeTLS("/etc/service-controller/console/tls.crt", "/etc/service-controller/console/tls.key")
			if err != nil {
				fmt.Println(err)
			}
		} else {
			err := s.ListenAndServe()
			if err != nil {
				fmt.Println(err)
			}
		}
	}()
	if *isProf {
		// serve only over localhost loopback
		go func() {
			if err := http.ListenAndServe("localhost:9970", nil); err != nil {
				log.Fatalf("failure running default http server for net/http/pprof: %s", err)
			}
		}()
	}

	if err = c.Run(stopCh); err != nil {
		log.Fatal("Error running Flow collector: ", err.Error())
	}

}
