package job

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8syaml "k8s.io/apimachinery/pkg/runtime/serializer/yaml"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/kubectl/pkg/cmd/portforward"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/templates"
	"sigs.k8s.io/yaml"

	"github.com/google/shlex"
	"github.com/ray-project/kuberay/kubectl-plugin/pkg/util"
	"github.com/ray-project/kuberay/kubectl-plugin/pkg/util/client"
	"github.com/spf13/cobra"

	rayv1api "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
)

const (
	dashboardAddr      = "http://localhost:8265"
	clusterTimeout     = 120.0
	portforwardtimeout = 60.0
)

type SubmitJobOptions struct {
	ioStreams          *genericiooptions.IOStreams
	configFlags        *genericclioptions.ConfigFlags
	RayJob             *unstructured.Unstructured
	submissionID       string
	entryPoint         string
	fileName           string
	workingDir         string
	runtimeEnv         string
	headers            string
	verify             string
	cluster            string
	runtimeEnvJson     string
	entryPointResource string
	metadataJson       string
	logStyle           string
	logColor           string
	entryPointCPU      float32
	entryPointGPU      float32
	entryPointMemory   int
	noWait             bool
}

type RayJob struct {
	*rayv1api.RayJob
}

var (
	jobSubmitLong = templates.LongDesc(`
		Submit ray job to ray cluster as one would using ray CLI e.g. 'ray job submit ENTRYPOINT'. Command supports all options that 'ray job submit' supports, except '--address'.
		If RayCluster is already setup, use 'kubectl ray session' instead.

		Command will apply RayJob CR and also submit the ray job. RayJob CR is required.
	`)

	jobSubmitExample = templates.Examples(`
		# Submit ray job with working-directory
		kubectl ray job submit -f rayjob.yaml --working-dir /path/to/working-dir/ -- python my_script.py

		# Submit ray job with runtime Env file and working directory
		kubectl ray job submit -f rayjob.yaml --working-dir /path/to/working-dir/ --runtime-env /runtimeEnv.yaml -- python my_script.py

		# Submit ray job with runtime Env file assuming runtime-env has working_dir set
		kubectl ray job submit -f rayjob.yaml --runtime-env path/to/runtimeEnv.yaml -- python my_script.py
	`)
)

func NewJobSubmitOptions(streams genericiooptions.IOStreams) *SubmitJobOptions {
	return &SubmitJobOptions{
		ioStreams:   &streams,
		configFlags: genericclioptions.NewConfigFlags(true),
	}
}

func NewJobSubmitCommand(streams genericclioptions.IOStreams) *cobra.Command {
	options := NewJobSubmitOptions(streams)
	cmdFactory := cmdutil.NewFactory(options.configFlags)

	cmd := &cobra.Command{
		Use:     "submit [OPTIONS] -f/--filename RAYJOB_YAML -- ENTRYPOINT",
		Short:   "Submit ray job to ray cluster",
		Long:    jobSubmitLong,
		Example: jobSubmitExample,
		RunE: func(cmd *cobra.Command, args []string) error {
			entryPointStart := cmd.ArgsLenAtDash()
			options.entryPoint = strings.Join(args[entryPointStart:], " ")
			if err := options.Complete(); err != nil {
				return err
			}
			if err := options.Validate(); err != nil {
				return err
			}
			return options.Run(cmd.Context(), cmdFactory)
		},
	}
	cmd.Flags().StringVarP(&options.fileName, "filename", "f", options.fileName, "Path and name of the Ray Job YAML file")
	cmd.Flags().StringVar(&options.submissionID, "submission-id", options.submissionID, "ID to specify for the ray job. If not provided, one will be generated")
	cmd.Flags().StringVar(&options.runtimeEnv, "runtime-env", options.runtimeEnv, "Path and name to the runtime env YAML file.")
	cmd.Flags().StringVar(&options.workingDir, "working-dir", options.workingDir, "Directory containing files that your job will run in")
	cmd.Flags().StringVar(&options.headers, "headers", options.headers, "Used to pass headers through http/s to Ray Cluster. Must be JSON formatting")
	cmd.Flags().StringVar(&options.runtimeEnvJson, "runtime-env-json", options.runtimeEnvJson, "JSON-serialized runtime_env dictionary. Precedence over ray job CR.")
	cmd.Flags().StringVar(&options.verify, "verify", options.verify, "Boolean indication to verify the server’s TLS certificate or a path to a file or directory of trusted certificates.")
	cmd.Flags().StringVar(&options.entryPointResource, "entrypoint-resources", options.entryPointResource, "JSON-serialized dictionary mapping resource name to resource quantity")
	cmd.Flags().StringVar(&options.metadataJson, "metadata-json", options.metadataJson, "JSON-serialized dictionary of metadata to attach to the job.")
	cmd.Flags().StringVar(&options.logStyle, "log-style", options.logStyle, "Specific to 'ray job submit'. Options are 'auto | record | pretty'")
	cmd.Flags().StringVar(&options.logColor, "log-clor", options.logColor, "Specifc to 'ray job submit'. Options are 'auto | false | true'")
	cmd.Flags().Float32Var(&options.entryPointCPU, "entrypoint-num-cpus", options.entryPointCPU, "Number of CPU reserved for the for the entrypoint command")
	cmd.Flags().Float32Var(&options.entryPointGPU, "entrypoint-num-gpus", options.entryPointGPU, "Number of GPU reserved for the for the entrypoint command")
	cmd.Flags().IntVar(&options.entryPointMemory, "entrypoint-memory", options.entryPointMemory, "Amount of memory reserved for the entrypoint command")
	cmd.Flags().BoolVar(&options.noWait, "no-wait", options.noWait, "If present, will not stream logs and wait for job to finish")
	err := cmd.MarkFlagRequired("filename")
	if err != nil {
		log.Fatalf("Failed to mark flag as required %v", err)
	}
	options.configFlags.AddFlags(cmd.Flags())
	return cmd
}

func (options *SubmitJobOptions) Complete() error {
	if *options.configFlags.Namespace == "" {
		*options.configFlags.Namespace = "default"
	}

	if len(options.runtimeEnv) > 0 {
		options.runtimeEnv = filepath.Clean(options.runtimeEnv)
	}

	options.fileName = filepath.Clean(options.fileName)
	return nil
}

func (options *SubmitJobOptions) Validate() error {
	// Overrides and binds the kube config then retrieves the merged result
	config, err := options.configFlags.ToRawKubeConfigLoader().RawConfig()
	if err != nil {
		return fmt.Errorf("Error retrieving raw config: %w", err)
	}
	if len(config.CurrentContext) == 0 {
		return fmt.Errorf("no context is currently set, use %q to select a new one", "kubectl config use-context <context>")
	}

	if len(options.runtimeEnv) > 0 {
		info, err := os.Stat(options.runtimeEnv)
		if os.IsNotExist(err) {
			return fmt.Errorf("Runtime Env file does not exist. Failed with: %w", err)
		} else if err != nil {
			return fmt.Errorf("Error occurred when checking runtime env file: %w", err)
		} else if !info.Mode().IsRegular() {
			return fmt.Errorf("Filename given is not a regular file. Failed with: %w", err)
		}

		runtimeEnvWorkingDir, err := runtimeEnvHasWorkingDir(options.runtimeEnv)
		if err != nil {
			return fmt.Errorf("Error while checking runtime env: %w", err)
		}
		if len(runtimeEnvWorkingDir) > 0 && options.workingDir == "" {
			options.workingDir = runtimeEnvWorkingDir
		}
	}

	info, err := os.Stat(options.fileName)
	if os.IsNotExist(err) {
		return fmt.Errorf("Ray Job file does not exist. Failed with: %w", err)
	} else if err != nil {
		return fmt.Errorf("Error occurred when checking ray job file: %w", err)
	} else if !info.Mode().IsRegular() {
		return fmt.Errorf("Filename given is not a regular file. Failed with: %w", err)
	}

	options.RayJob, err = decodeRayJobYaml(options.fileName)
	if err != nil {
		return fmt.Errorf("Failed to decode RayJob Yaml: %w", err)
	}

	submissionMode, ok := options.RayJob.Object["spec"].(map[string]interface{})["submissionMode"]
	if !ok {
		return fmt.Errorf("RayJob does not have `submissionMode` field set")
	}
	if submissionMode != nil {
		if submissionMode != "InteractiveMode" {
			return fmt.Errorf("Submission mode of the Ray Job is not supported")
		}
	} else {
		return fmt.Errorf("Submission mode must be set to 'InteractiveMode'")
	}

	runtimeEnvYaml, ok := options.RayJob.Object["spec"].(map[string]interface{})["runtimeEnvYAML"].(string)
	if ok && options.runtimeEnv == "" && options.runtimeEnvJson == "" {
		runtimeJson, err := yaml.YAMLToJSON([]byte(runtimeEnvYaml))
		if err != nil {
			return fmt.Errorf("Failed to convert runtime env to json: %w", err)
		}
		options.runtimeEnvJson = string(runtimeJson)
	}

	if options.workingDir == "" {
		return fmt.Errorf("working directory is required, use --working-dir or set with runtime env")
	}

	// Changed working dir clean to here instead of complete since calling Clean on empty string return "." and it would be dificult to determine if that is actually user input or not.
	options.workingDir = filepath.Clean(options.workingDir)
	return nil
}

func (options *SubmitJobOptions) Run(ctx context.Context, factory cmdutil.Factory) error {
	k8sClients, err := client.NewClient(factory)
	if err != nil {
		return fmt.Errorf("failed to initialize clientset: %w", err)
	}

	// createdRayJob, err = k8sClients.CreateRayCustomResource(ctx, util.RayJob, options.configFlags.Namespace, unstructuredRayjob)
	options.RayJob, err = k8sClients.DynamicClient().Resource(util.RayJobGVR).Namespace(*options.configFlags.Namespace).Create(ctx, options.RayJob, v1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("Error when creating RayJob CR: %w", err)
	}
	fmt.Printf("Submitted RayJob %s.\n", options.RayJob.GetName())

	if len(options.RayJob.GetName()) > 0 {
		// Add timeout?
		for options.RayJob.Object["status"] == nil {
			options.RayJob, err = k8sClients.DynamicClient().Resource(util.RayJobGVR).Namespace(*options.configFlags.Namespace).Get(ctx, options.RayJob.GetName(), v1.GetOptions{})
			if err != nil {
				return fmt.Errorf("Failed to get Ray Job status")
			}
			time.Sleep(2 * time.Second)
		}
		clusterName, ok := options.RayJob.Object["status"].(map[string]interface{})["rayClusterName"].(string)
		if !ok {
			return fmt.Errorf("Unable to find ray cluster status")
		}
		if len(clusterName) == 0 {
			return fmt.Errorf("No cluster name available even after status of Ray Job is set")
		}
		options.cluster = clusterName
	} else {
		return fmt.Errorf("Unknown cluster and did not provide Ray Job. One of the fields must be set")
	}

	// Wait til the cluster is ready
	var clusterReady bool
	clusterWaitStartTime := time.Now()
	currTime := clusterWaitStartTime
	fmt.Printf("Waiting for RayCluster\n")
	fmt.Printf("Checking Cluster Status for cluster %s...\n", options.cluster)
	for !clusterReady && currTime.Sub(clusterWaitStartTime).Seconds() <= clusterTimeout {
		time.Sleep(2 * time.Second)
		currCluster, err := k8sClients.DynamicClient().Resource(util.RayClusterGVR).Namespace(*options.configFlags.Namespace).Get(ctx, options.cluster, v1.GetOptions{})
		if err != nil {
			return fmt.Errorf("Failed to get cluster information with error: %w", err)
		}
		clusterReady, err = isRayClusterReady(currCluster)
		if err != nil {
			err = fmt.Errorf("Cluster is not ready: %w", err)
			fmt.Println(err)
		}
		currTime = time.Now()
	}

	if !clusterReady {
		fmt.Printf("Deleting RayJob...\n")
		err = k8sClients.DynamicClient().Resource(util.RayJobGVR).Namespace(*options.configFlags.Namespace).Delete(ctx, options.RayJob.GetName(), v1.DeleteOptions{})
		if err != nil {
			return fmt.Errorf("Failed to clean up ray job after time out.: %w", err)
		}
		fmt.Printf("Cleaned Up RayJob: %s\n", options.RayJob.GetName())

		return fmt.Errorf("Timed out waiting for cluster")
	}

	svcName, err := k8sClients.GetRayHeadSvcName(ctx, *options.configFlags.Namespace, util.RayCluster, options.cluster)
	if err != nil {
		return fmt.Errorf("Failed to find service name: %w", err)
	}

	// start port forward section
	portForwardCmd := portforward.NewCmdPortForward(factory, *options.ioStreams)
	portForwardCmd.SetArgs([]string{"service/" + svcName, fmt.Sprintf("%d:%d", 8265, 8265)})

	// create new context for port-forwarding so we can cancel the context to stop the port forwarding only
	portforwardctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		fmt.Printf("Port Forwarding service %s\n", svcName)
		if err := portForwardCmd.ExecuteContext(portforwardctx); err != nil {
			log.Fatalf("Error occurred while port-forwarding Ray dashboard: %v", err)
		}
	}()

	// Wait for port forward to be ready
	var portforwardReady bool
	portforwardWaitStartTime := time.Now()
	currTime = portforwardWaitStartTime

	portforwardCheckRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, dashboardAddr, nil)
	if err != nil {
		return fmt.Errorf("Error occurred when trying to create request to probe cluster endpoint: %w", err)
	}
	httpClient := http.Client{
		Timeout: 5 * time.Second,
	}
	fmt.Printf("Waiting for portforwarding...")
	for !portforwardReady && currTime.Sub(portforwardWaitStartTime).Seconds() <= portforwardtimeout {
		time.Sleep(2 * time.Second)
		rayDashboardResponse, err := httpClient.Do(portforwardCheckRequest)
		if err != nil {
			err = fmt.Errorf("Error occurred when waiting for portforwarding: %w", err)
			fmt.Println(err)
		}
		if rayDashboardResponse.StatusCode >= 200 && rayDashboardResponse.StatusCode < 300 {
			portforwardReady = true
		}
		rayDashboardResponse.Body.Close()
		currTime = time.Now()
	}
	if !portforwardReady {
		return fmt.Errorf("Timed out waiting for port forwarding")
	}
	fmt.Printf("Portforwarding started on %s\n", dashboardAddr)

	// Submitting ray job to cluster
	raySubmitCmd, err := options.raySubmitCmd()
	if err != nil {
		return fmt.Errorf("failed to create Ray submit command with error: %w", err)
	}
	fmt.Printf("Ray command: %v\n", raySubmitCmd)
	cmd := exec.Command(raySubmitCmd[0], raySubmitCmd[1:]...) //nolint:gosec // command is sanitized in raySubmitCmd() and file paths are cleaned in Complete()

	// Get the outputs/pipes for `ray job submit` outputs
	rayCmdStdOut, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("Error while setting up `ray job submit` stdout: %w", err)
	}
	rayCmdStdErr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("Error while setting up `ray job submit` stderr: %w", err)
	}

	go func() {
		fmt.Printf("Running ray submit job command...\n")
		err := cmd.Start()
		if err != nil {
			log.Fatalf("error occurred while running command %s: %v", fmt.Sprint(raySubmitCmd), err)
		}
	}()

	var rayJobID string
	if options.submissionID != "" {
		rayJobID = options.submissionID
	}
	// Make channel for retrieving rayJobID from output
	rayJobIDChan := make(chan string)

	rayCmdStdOutScanner := bufio.NewScanner(rayCmdStdOut)
	rayCmdStdErrScanner := bufio.NewScanner(rayCmdStdErr)
	go func() {
		for {
			currStdToken := rayCmdStdOutScanner.Text()
			// Running under assumption that scanner does not break up ray job name
			if currStdToken != "" && rayJobID == "" && strings.Contains(currStdToken, "raysubmit") {
				regexExp := regexp.MustCompile(`'([^']*raysubmit[^']*)'`)
				// Search for rayjob name. Returns at least two string, first one has single quotes and second string does not have single quotes
				match := regexExp.FindStringSubmatch(currStdToken)
				if len(match) > 1 {
					rayJobIDChan <- match[1]
				}
			}
			if currStdToken != "" {
				fmt.Println(currStdToken)
			}
			scanNotDone := rayCmdStdOutScanner.Scan()
			if !scanNotDone {
				break
			}
		}
	}()
	go func() {
		for {
			currErrToken := rayCmdStdErrScanner.Text()
			if currErrToken != "" {
				fmt.Fprintf(options.ioStreams.ErrOut, "%s\n", currErrToken)
			}
			scanNotDone := rayCmdStdErrScanner.Scan()
			if !scanNotDone {
				break
			}
		}
	}()

	// Wait till rayJobID is populated
	if rayJobID == "" {
		rayJobID = <-rayJobIDChan
	}
	// Add annotation to RayJob with the correct ray job id and update the CR
	options.RayJob, err = k8sClients.DynamicClient().Resource(util.RayJobGVR).Namespace(*options.configFlags.Namespace).Get(ctx, options.RayJob.GetName(), v1.GetOptions{})
	if err != nil {
		return fmt.Errorf("Failed to get latest version of Ray Job")
	}

	rayJobAnnotations := options.RayJob.GetAnnotations()
	if rayJobAnnotations == nil {
		rayJobAnnotations = make(map[string]string)
	}

	rayJobAnnotations["ray.io/ray-job-submission-id"] = rayJobID
	options.RayJob.SetAnnotations(rayJobAnnotations)

	_, err = k8sClients.DynamicClient().Resource(util.RayJobGVR).Namespace(*options.configFlags.Namespace).Update(ctx, options.RayJob, v1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("Error occurred when trying to add job ID to rayJob: %w", err)
	}

	// Wait for ray job submit to finish.
	err = cmd.Wait()
	if err != nil {
		return fmt.Errorf("Error occurred with ray job submit: %w", err)
	}
	return nil
}

func (options *SubmitJobOptions) raySubmitCmd() ([]string, error) {
	raySubmitCmd := []string{"ray", "job", "submit", "--address", dashboardAddr}

	if len(options.runtimeEnv) > 0 {
		raySubmitCmd = append(raySubmitCmd, "--runtime-env", options.runtimeEnv)
	}
	if len(options.runtimeEnvJson) > 0 {
		raySubmitCmd = append(raySubmitCmd, "--runtime-env-json", options.runtimeEnvJson)
	}
	if len(options.submissionID) > 0 {
		raySubmitCmd = append(raySubmitCmd, "--submission-id", options.submissionID)
	}
	if options.entryPointCPU > 0 {
		raySubmitCmd = append(raySubmitCmd, "--entrypoint-num-cpus", fmt.Sprintf("%f", options.entryPointCPU))
	}
	if options.entryPointGPU > 0 {
		raySubmitCmd = append(raySubmitCmd, "--entrypoint-num-gpus", fmt.Sprintf("%f", options.entryPointGPU))
	}
	if options.entryPointMemory > 0 {
		raySubmitCmd = append(raySubmitCmd, "--entrypoint-memory", fmt.Sprintf("%d", options.entryPointMemory))
	}
	if len(options.entryPointResource) > 0 {
		raySubmitCmd = append(raySubmitCmd, "--entrypoint-resource", options.entryPointResource)
	}
	if len(options.metadataJson) > 0 {
		raySubmitCmd = append(raySubmitCmd, "--metadata-json", options.metadataJson)
	}
	if options.noWait {
		raySubmitCmd = append(raySubmitCmd, "--no-wait")
	}
	if len(options.headers) > 0 {
		raySubmitCmd = append(raySubmitCmd, "--headers", options.headers)
	}
	if len(options.verify) > 0 {
		raySubmitCmd = append(raySubmitCmd, "--verify", options.verify)
	}
	if len(options.logStyle) > 0 {
		raySubmitCmd = append(raySubmitCmd, "--log-style", options.logStyle)
	}
	if len(options.logColor) > 0 {
		raySubmitCmd = append(raySubmitCmd, "--log-color", options.logColor)
	}

	raySubmitCmd = append(raySubmitCmd, "--working-dir", options.workingDir)

	raySubmitCmd = append(raySubmitCmd, "--")
	// Sanitize entrypoint
	entryPointSanitized, err := shlex.Split(options.entryPoint)
	if err != nil {
		return nil, err
	}
	raySubmitCmd = append(raySubmitCmd, entryPointSanitized...)

	return raySubmitCmd, nil
}

// Decode rayjob yaml if we decide to submit job using kube client
func decodeRayJobYaml(rayJobFilePath string) (*unstructured.Unstructured, error) {
	decodedRayJob := &unstructured.Unstructured{}
	decUnstructured := k8syaml.NewDecodingSerializer(unstructured.UnstructuredJSONScheme)

	rayJobYamlContent, err := os.ReadFile(rayJobFilePath)
	if err != nil {
		return nil, err
	}

	_, _, err = decUnstructured.Decode(rayJobYamlContent, nil, decodedRayJob)
	if err != nil {
		return nil, err
	}

	return decodedRayJob, nil
}

func runtimeEnvHasWorkingDir(runtimePath string) (string, error) {
	runtimeEnvFileContent, err := os.ReadFile(runtimePath)
	if err != nil {
		return "", err
	}

	var runtimeEnvYaml map[string]interface{}
	err = yaml.Unmarshal(runtimeEnvFileContent, &runtimeEnvYaml)
	if err != nil {
		return "", err
	}

	workingDir := runtimeEnvYaml["working_dir"].(string)
	if workingDir != "" {
		return workingDir, nil
	}

	return "", nil
}

func isRayClusterReady(rayCluster *unstructured.Unstructured) (bool, error) {
	var isReady bool
	rayClusterConditions, ok := rayCluster.Object["status"].(map[string]interface{})["conditions"].([]v1.Condition)
	if ok {
		isReady = meta.IsStatusConditionTrue(rayClusterConditions, "Ready")
	}

	rayClusterState, ok := rayCluster.Object["status"].(map[string]interface{})["state"].(string)
	if ok {
		isReady = isReady || rayClusterState == "ready"
	}

	if isReady {
		return isReady, nil
	}

	return false, errors.New("Cannot determine cluster state")
}