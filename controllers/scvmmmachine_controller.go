/*
Copyright 2021.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	// "k8s.io/apimachinery/pkg/runtime"
	// "sigs.k8s.io/cluster-api/controllers/remote"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/predicates"

	infrav1 "github.com/willemm/cluster-api-provider-scvmm/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"

	// "encoding/base64"
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/willemm/winrm"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// Creation started
	VmCreated clusterv1.ConditionType = "VmCreated"
	// VM running
	VmRunning clusterv1.ConditionType = "VmRunning"

	// Cluster-Api related statuses
	WaitingForClusterInfrastructureReason = "WaitingForClusterInfrastructure"
	WaitingForControlPlaneAvailableReason = "WaitingForControlplaneAvailable"
	WaitingForBootstrapDataReason         = "WaitingForBootstrapData"
	WaitingForOwnerReason                 = "WaitingForOwner"
	ClusterNotAvailableReason             = "ClusterNotAvailable"
	MissingClusterReason                  = "MissingCluster"

	VmCreatingReason = "VmCreating"
	VmUpdatingReason = "VmUpdating"
	VmStartingReason = "VmStarting"
	VmDeletingReason = "VmDeleting"
	VmFailedReason   = "VmFailed"

	MachineFinalizer = "scvmmmachine.finalizers.cluster.x-k8s.io"
)

// ScvmmMachineReconciler reconciles a ScvmmMachine object
type ScvmmMachineReconciler struct {
	client.Client
	Log logr.Logger
	// Tracker *remote.ClusterCacheTracker
	// Scheme  *runtime.Scheme
}

// Are global variables bad? Dunno, this one seems fine because it caches an env var
var ExtraDebug bool = false

// The result (passed as json) of a call to Scvmm scripts
type VMResult struct {
	Cloud          string
	Name           string
	Hostname       string
	Status         string
	Memory         int
	CpuCount       int
	VirtualNetwork string
	IPv4Addresses  []string
	VirtualDisks   []struct {
		Size        int64
		MaximumSize int64
	}
	BiosGuid     string
	Id           string
	VMId         string
	Error        string
	ScriptErrors string
	Message      string
	CreationTime metav1.Time
	ModifiedTime metav1.Time
}

type VMSpecResult struct {
	infrav1.ScvmmMachineSpec
	Error        string
	ScriptErrors string
	Message      string
}

func getFuncScript(provider *infrav1.ScvmmProviderSpec) ([]byte, error) {
	var funcScripts map[string][]byte
	scriptfiles, err := filepath.Glob(os.Getenv("SCRIPT_DIR") + "/*.ps1")
	if err != nil {
		return nil, fmt.Errorf("error scanning script dir %s: %v", os.Getenv("SCRIPT_DIR"), err)
	}
	for _, file := range scriptfiles {
		name := strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
		content, err := ioutil.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("error reading script file %s: %v", file, err)
		}
		funcScripts[name] = content
	}
	for name, content := range provider.ExtraFunctions {
		funcScripts[name] = []byte(content)
	}
	var functionScripts bytes.Buffer
	functionScripts.WriteString("$ProgressPreference = 'SilentlyContinue'\n" +
		"$WarningPreference = 'SilentlyContinue'\n" +
		"$VerbosePreference = 'SilentlyContinue'\n" +
		"$InformationPreference = 'SilentlyContinue'\n" +
		"$DebugPreference = 'SilentlyContinue'\n\n")
	for name, content := range funcScripts {
		functionScripts.WriteString("function " + name + " {\n")
		functionScripts.Write(content)
		functionScripts.WriteString("}\n\n")
	}
	return functionScripts.Bytes(), nil
}

func (r *ScvmmMachineReconciler) getProvider(ctx context.Context, scvmmCluster *infrav1.ScvmmCluster, scvmmMachine *infrav1.ScvmmMachine) (*infrav1.ScvmmProviderSpec, error) {
	var providerRef *corev1.ObjectReference
	if scvmmCluster == nil {
		if scvmmMachine.Spec.CloudInit != nil {
			providerRef = scvmmMachine.Spec.CloudInit.ProviderRef
		}
	} else {
		providerRef = scvmmCluster.Spec.ProviderRef
	}
	provider := &infrav1.ScvmmProvider{}
	if providerRef != nil {
		key := client.ObjectKey{Namespace: providerRef.Namespace, Name: providerRef.Name}
		if key.Namespace == "" {
			key.Namespace = scvmmCluster.Namespace
		}
		if err := r.Client.Get(ctx, key, provider); err != nil {
			return nil, fmt.Errorf("Failed to get ScvmmProvider: %v", err)
		}
	}
	p := &provider.Spec

	// Set defaults
	if p.ScvmmHost == "" {
		p.ScvmmHost = os.Getenv("SCVMM_HOST")
		if p.ScvmmHost == "" {
			return nil, fmt.Errorf("missing required value ScvmmHost")
		}
	}
	if p.ExecHost == "" {
		p.ExecHost = os.Getenv("SCVMM_EXECHOST")
		// Disable scvmm connect script ??? (untested)
		p.ExtraFunctions["ConnectSCVMM"] = ""
		p.ExecHost = p.ScvmmHost
	}
	if p.ScvmmLibraryISOs == "" {
		p.ScvmmLibraryISOs = os.Getenv("SCVMM_LIBRARY")
		if p.ScvmmLibraryISOs == "" {
			p.ScvmmLibraryISOs = `\\` + p.ScvmmHost + `\MSSCVMMLibrary\ISOs\clond-init`
		}
	}
	if p.ADServer == "" {
		p.ADServer = os.Getenv("ACTIVEDIRECTORY_SERVER")
	}
	if p.SecretRef != nil {
		creds := &corev1.Secret{}
		key := client.ObjectKey{Namespace: provider.Namespace, Name: p.SecretRef.Name}
		if err := r.Client.Get(ctx, key, creds); err != nil {
			return nil, fmt.Errorf("Failed to get credential secretref: %v", err)
		}
		if value, ok := creds.Data["username"]; ok {
			p.ScvmmUsername = string(value)
		}
		if value, ok := creds.Data["password"]; ok {
			p.ScvmmPassword = string(value)
		}
	}
	if p.ScvmmUsername == "" {
		p.ScvmmUsername = os.Getenv("SCVMM_USERNAME")
	}
	if p.ScvmmPassword == "" {
		p.ScvmmPassword = os.Getenv("SCVMM_PASSWORD")
	}
	return p, nil
}

// Create a winrm powershell session and seed with the function script
func createWinrmCmd(provider *infrav1.ScvmmProviderSpec, log logr.Logger) (*winrm.DirectCommand, error) {
	functionScript, err := getFuncScript(provider)
	if err != nil {
		return &winrm.DirectCommand{}, err
	}
	endpoint := winrm.NewEndpoint(provider.ExecHost, 5985, false, false, nil, nil, nil, 0)
	params := winrm.DefaultParameters
	params.TransportDecorator = func() winrm.Transporter { return &winrm.ClientNTLM{} }

	if ExtraDebug {
		log.V(1).Info("Creating WinRM connection", provider.ExecHost, 5985)
	}
	client, err := winrm.NewClientWithParameters(endpoint, provider.ScvmmUsername, provider.ScvmmPassword, params)
	if err != nil {
		return &winrm.DirectCommand{}, errors.Wrap(err, "Creating winrm client")
	}
	if ExtraDebug {
		log.V(1).Info("Creating WinRM shell")
	}
	shell, err := client.CreateShell()
	if err != nil {
		return &winrm.DirectCommand{}, errors.Wrap(err, "Creating winrm shell")
	}
	if ExtraDebug {
		log.V(1).Info("Starting WinRM powershell.exe")
	}
	cmd, err := shell.ExecuteDirect("powershell.exe", "-NonInteractive", "-NoProfile", "-Command", "-")
	if err != nil {
		return &winrm.DirectCommand{}, errors.Wrap(err, "Creating winrm powershell")
	}
	if ExtraDebug {
		log.V(1).Info("Sending WinRM ping")
		if err := cmd.SendCommand("Write-Host 'OK'"); err != nil {
			cmd.Close()
			return &winrm.DirectCommand{}, errors.Wrap(err, "Sending powershell functions post")
		}
		log.V(1).Info("Getting WinRM ping")
		stdout, stderr, _, _, err := cmd.ReadOutput()
		if err != nil {
			cmd.Close()
			return &winrm.DirectCommand{}, errors.Wrap(err, "Reading powershell functions post")
		}
		log.V(1).Info("Got WinRM ping", "stdout", string(stdout), "stderr", string(stderr))
		if strings.TrimSpace(string(stdout)) != "OK" {
			cmd.Close()
			return &winrm.DirectCommand{}, errors.New("Powershell functions result: " + string(stdout) + " (ERR=" + string(stderr))
		}
	}
	if ExtraDebug {
		log.V(1).Info("Sending WinRM function script")
	}
	if err := cmd.SendInput(functionScript, false); err != nil {
		cmd.Close()
		return &winrm.DirectCommand{}, errors.Wrap(err, "Sending powershell functions")
	}
	if ExtraDebug {
		log.V(1).Info("Calling WinRM function ConnectSCVMM")
	}
	if err := cmd.SendCommand("ConnectSCVMM -Host '%s' -Username '%s' -Password '%s'", provider.ScvmmHost, provider.ScvmmUsername, provider.ScvmmPassword); err != nil {
		cmd.Close()
		return &winrm.DirectCommand{}, errors.Wrap(err, "Sending powershell functions post")
	}
	if ExtraDebug {
		log.V(1).Info("Sending WinRM ping")
	}
	if err := cmd.SendCommand("Write-Host 'OK'"); err != nil {
		cmd.Close()
		return &winrm.DirectCommand{}, errors.Wrap(err, "Sending powershell functions post")
	}
	if ExtraDebug {
		log.V(1).Info("Getting WinRM ping")
	}
	stdout, stderr, _, _, err := cmd.ReadOutput()
	if err != nil {
		cmd.Close()
		return &winrm.DirectCommand{}, errors.Wrap(err, "Reading powershell functions post")
	}
	if ExtraDebug {
		log.V(1).Info("Got WinRM ping", "stdout", string(stdout), "stderr", string(stderr))
	}
	if strings.TrimSpace(string(stdout)) != "OK" {
		cmd.Close()
		return &winrm.DirectCommand{}, errors.New("Powershell functions result: " + string(stdout) + " (ERR=" + string(stderr))
	}
	return cmd, nil
}

func getWinrmResult(cmd *winrm.DirectCommand, log logr.Logger) (VMResult, error) {
	stdout, stderr, _, _, err := cmd.ReadOutput()
	if err != nil {
		return VMResult{}, errors.Wrap(err, "Failed to read output")
	}
	if ExtraDebug {
		log.V(1).Info("Got WinRM Result", "stdout", string(stdout), "stderr", string(stderr))
	}
	var res VMResult
	if err := json.Unmarshal(stdout, &res); err != nil {
		return VMResult{}, errors.Wrap(err, "Decode result error: "+string(stdout)+
			"  (stderr="+string(stderr)+")")
	}
	return res, nil
}

func sendWinrmCommand(log logr.Logger, cmd *winrm.DirectCommand, command string, args ...interface{}) (VMResult, error) {
	if ExtraDebug {
		log.V(1).Info("Sending WinRM command", "command", command, "args", args,
			"cmdline", fmt.Sprintf(command+"\n", args...))
	}
	if err := cmd.SendCommand(command, args...); err != nil {
		return VMResult{}, err
	}
	return getWinrmResult(cmd, log)
}

func getWinrmSpecResult(cmd *winrm.DirectCommand, log logr.Logger) (VMSpecResult, error) {
	stdout, stderr, _, _, err := cmd.ReadOutput()
	if err != nil {
		return VMSpecResult{}, errors.Wrap(err, "Failed to read output")
	}
	if ExtraDebug {
		log.V(1).Info("Got WinRMSpec Result", "stdout", string(stdout), "stderr", string(stderr))
	}
	var res VMSpecResult
	if err := json.Unmarshal(stdout, &res); err != nil {
		return VMSpecResult{}, errors.Wrap(err, "Decode result error: "+string(stdout)+
			"  (stderr="+string(stderr)+")")
	}
	return res, nil
}

func escapeSingleQuotes(str string) string {
	return strings.Replace(str, `'`, `''`, -1)
}

func escapeSingleQuotesArray(str []string) string {
	var res strings.Builder
	if len(str) == 0 {
		return ""
	}
	res.WriteString("'")
	res.WriteString(strings.Replace(str[0], `'`, `''`, -1))
	for s := range str[1:] {
		res.WriteString("','")
		res.WriteString(strings.Replace(str[s], `'`, `''`, -1))
	}
	res.WriteString("'")
	return res.String()
}

func sendWinrmSpecCommand(log logr.Logger, cmd *winrm.DirectCommand, command string, scvmmMachine *infrav1.ScvmmMachine) (VMSpecResult, error) {
	specjson, err := json.Marshal(scvmmMachine.Spec)
	if err != nil {
		return VMSpecResult{ScriptErrors: "Error encoding spec"}, errors.Wrap(err, "encoding spec")
	}
	metajson, err := json.Marshal(scvmmMachine.ObjectMeta)
	if err != nil {
		return VMSpecResult{ScriptErrors: "Error encoding metadata"}, errors.Wrap(err, "encoding metadata")
	}
	if ExtraDebug {
		log.V(1).Info("Sending WinRM command", "command", command, "spec", scvmmMachine.Spec,
			"metadata", scvmmMachine.ObjectMeta,
			"cmdline", fmt.Sprintf(command+" -spec '%s' -metadata '%s'\n",
				escapeSingleQuotes(string(specjson)),
				escapeSingleQuotes(string(metajson))))
	}
	if err := cmd.SendCommand(command+" -spec '%s' -metadata '%s'",
		escapeSingleQuotes(string(specjson)),
		escapeSingleQuotes(string(metajson))); err != nil {
		return VMSpecResult{ScriptErrors: "Error executing command"}, errors.Wrap(err, "sending command")
	}
	return getWinrmSpecResult(cmd, log)
}

// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=scvmmmachines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=scvmmmachines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=scvmmmachines/finalizers,verbs=update
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters;machines,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets;,verbs=get;list;watch

func (r *ScvmmMachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("scvmmmachine", req.NamespacedName)

	log.V(1).Info("Fetching scvmmmachine")
	// Fetch the instance
	scvmmMachine := &infrav1.ScvmmMachine{}
	if err := r.Get(ctx, req.NamespacedName, scvmmMachine); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Workaround bug in patchhelper
	if scvmmMachine.Spec.Disks != nil {
		for i, d := range scvmmMachine.Spec.Disks {
			if d.Size == nil {
				scvmmMachine.Spec.Disks[i].Size = resource.NewQuantity(0, resource.BinarySI)
			}
		}
	}

	patchHelper, err := patch.NewHelper(scvmmMachine, r)
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "Get patchhelper")
	}

	// Handle deleted machines
	// NB: The reference implementation handles deletion at the end of this function, but that seems wrogn
	//     because it could be that the machine, cluster, etc is gone and also it tries to add finalizers
	deleting := false
	if !scvmmMachine.ObjectMeta.DeletionTimestamp.IsZero() {
		deleting = true
	}

	var cluster *clusterv1.Cluster
	var machine *clusterv1.Machine
	var scvmmCluster *infrav1.ScvmmCluster
	// If the user provides a cloudInit section in the machine, assume it's a standalone machine
	// Otherwise get the owning machine, cluster, etc.
	if scvmmMachine.Spec.CloudInit == nil {
		log.V(1).Info("Fetching machine")
		// Fetch the Machine.
		machine, err = util.GetOwnerMachine(ctx, r.Client, scvmmMachine.ObjectMeta)
		if err != nil {
			return ctrl.Result{}, errors.Wrap(err, "Get owner machine")
		}
		if machine == nil {
			log.Info("Waiting for Machine Controller to set OwnerRef on ScvmmMachine")
			return patchReasonCondition(ctx, log, patchHelper, scvmmMachine, 0, nil, VmCreated, WaitingForOwnerReason, "")
		}

		log = log.WithValues("machine", machine.Name)

		log.V(1).Info("Fetching cluster")
		// Fetch the Cluster.
		cluster, err = util.GetClusterFromMetadata(ctx, r.Client, machine.ObjectMeta)
		if err != nil {
			log.Info("ScvmmMachine owner Machine is missing cluster label or cluster does not exist")
			return patchReasonCondition(ctx, log, patchHelper, scvmmMachine, 0, err, VmCreated, MissingClusterReason, "ScvmmMachine owner Machine is missing cluster label or cluster does not exist")
		}
		if cluster == nil {
			log.Info(fmt.Sprintf("Please associate this machine with a cluster using the label %s: <name of cluster>", clusterv1.ClusterLabelName))
			return patchReasonCondition(ctx, log, patchHelper, scvmmMachine, 0, nil, VmCreated, MissingClusterReason, "Please associate this machine with a cluster using the label %s: <name of cluster>", clusterv1.ClusterLabelName)
		}

		log = log.WithValues("cluster", cluster.Name)

		log.V(1).Info("Fetching scvmmcluster")
		// Fetch the Scvmm Cluster.
		scvmmClusterName := client.ObjectKey{
			Namespace: scvmmMachine.Namespace,
			Name:      cluster.Spec.InfrastructureRef.Name,
		}
		if err := r.Client.Get(ctx, scvmmClusterName, scvmmCluster); err != nil {
			log.Info("ScvmmCluster is not available yet")
			return patchReasonCondition(ctx, log, patchHelper, scvmmMachine, 0, nil, VmCreated, ClusterNotAvailableReason, "")
		}

		log = log.WithValues("scvmm-cluster", scvmmCluster.Name)

		// Check if the infrastructure is ready, otherwise return and wait for the cluster object to be updated
		if !cluster.Status.InfrastructureReady {
			log.Info("Waiting for ScvmmCluster Controller to create cluster infrastructure")
			return patchReasonCondition(ctx, log, patchHelper, scvmmMachine, 0, nil, VmCreated, WaitingForClusterInfrastructureReason, "")
		}
	}

	log.V(1).Info("Check finalizer")
	// Add finalizer.  Apparently we should return here to avoid a race condition
	// (Presumably the change/patch will trigger another reconciliation so it continues)
	if !deleting && !controllerutil.ContainsFinalizer(scvmmMachine, MachineFinalizer) {
		log.V(1).Info("Add finalizer")
		controllerutil.AddFinalizer(scvmmMachine, MachineFinalizer)
		if perr := patchScvmmMachine(ctx, patchHelper, scvmmMachine); perr != nil {
			log.Error(perr, "Failed to patch scvmmMachine", "scvmmmachine", scvmmMachine)
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	provider, err := r.getProvider(ctx, scvmmCluster, scvmmMachine)
	if err != nil {
		return ctrl.Result{}, nil
	}

	if deleting {
		return r.reconcileDelete(ctx, patchHelper, provider, scvmmMachine)
	} else {
		// Handle non-deleted machines
		return r.reconcileNormal(ctx, patchHelper, cluster, provider, machine, scvmmMachine)
	}
}

func (r *ScvmmMachineReconciler) reconcileNormal(ctx context.Context, patchHelper *patch.Helper, cluster *clusterv1.Cluster, provider *infrav1.ScvmmProviderSpec, machine *clusterv1.Machine, scvmmMachine *infrav1.ScvmmMachine) (res ctrl.Result, retErr error) {
	log := r.Log.WithValues("scvmmmachine", scvmmMachine.Name)

	log.Info("Doing reconciliation of ScvmmMachine")
	cmd, err := createWinrmCmd(provider, log)
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "Winrm")
	}
	defer cmd.Close()
	var vm VMResult
	if scvmmMachine.Spec.VMName != "" {
		log.V(1).Info("Running GetVM")
		vm, err = sendWinrmCommand(log, cmd, "GetVM -VMName '%s'", escapeSingleQuotes(scvmmMachine.Spec.VMName))
		if err != nil {
			return ctrl.Result{}, errors.Wrap(err, "Failed to get vm")
		}
		log.V(1).Info("GetVM result", "vm", vm)
	}
	if vm.Name == "" {
		vmName := scvmmMachine.Spec.VMName
		if vmName == "" {
			log.V(1).Info("Call GenerateVMName")
			newspec, err := sendWinrmSpecCommand(log, cmd, "GenerateVMName", scvmmMachine)
			if err != nil {
				return patchReasonCondition(ctx, log, patchHelper, scvmmMachine, 0, err, VmCreated, VmFailedReason, "Failed generate vmname")
			}
			log.V(1).Info("GenerateVMName result", "newspec", newspec)
			if newspec.Error != "" {
				return patchReasonCondition(ctx, log, patchHelper, scvmmMachine, 0, err, VmCreated, VmFailedReason, "Failed generate vmname: "+newspec.Message)
			}
			if newspec.VMName == "" {
				return patchReasonCondition(ctx, log, patchHelper, scvmmMachine, 0, err, VmCreated, VmFailedReason, "Failed generate vmname: "+newspec.Message)
			}
			vmName = newspec.VMName
		}
		spec := scvmmMachine.Spec
		adspec := spec.ActiveDirectory
		if adspec != nil {
			log.V(1).Info("Call CreateADComputer")
			vm, err = sendWinrmCommand(log, cmd, "CreateADComputer -Name '%s' -OUPath '%s' -DomainController '%s' -Description '%s' -MemberOf @(%s)",
				escapeSingleQuotes(vmName),
				escapeSingleQuotes(adspec.OUPath),
				escapeSingleQuotes(adspec.DomainController),
				escapeSingleQuotes(adspec.Description),
				escapeSingleQuotesArray(adspec.MemberOf))
			if err != nil {
				return patchReasonCondition(ctx, log, patchHelper, scvmmMachine, 0, err, VmCreated, VmFailedReason, "Failed to create AD entry")
			}
			log.V(1).Info("CreateADComputer Result", "vm", vm)
			if vm.Error != "" {
				return patchReasonCondition(ctx, log, patchHelper, scvmmMachine, 60, nil, VmCreated, VmFailedReason, "Failed to create AD entry: %s", vm.Error)
			}
		}
		log.V(1).Info("Call CreateVM")
		diskjson, err := makeDisksJSON(spec.Disks)
		if err != nil {
			return patchReasonCondition(ctx, log, patchHelper, scvmmMachine, 0, err, VmCreated, VmFailedReason, "Failed to create vm")
		}
		vm, err = sendWinrmCommand(log, cmd, "CreateVM -Cloud '%s' -HostGroup '%s' -VMName '%s' -VMTemplate '%s' -Memory %d -CPUCount %d -Disks '%s' -VMNetwork '%s' -HardwareProfile '%s' -Description '%s' -StartAction '%s' -StopAction '%s'",
			escapeSingleQuotes(spec.Cloud),
			escapeSingleQuotes(spec.HostGroup),
			escapeSingleQuotes(vmName),
			escapeSingleQuotes(spec.VMTemplate),
			(spec.Memory.Value() / 1024 / 1024),
			spec.CPUCount,
			escapeSingleQuotes(string(diskjson)),
			escapeSingleQuotes(spec.VMNetwork),
			escapeSingleQuotes(spec.HardwareProfile),
			escapeSingleQuotes(spec.Description),
			escapeSingleQuotes(spec.StartAction),
			escapeSingleQuotes(spec.StopAction))
		if err != nil {
			return patchReasonCondition(ctx, log, patchHelper, scvmmMachine, 0, err, VmCreated, VmFailedReason, "Failed to create vm")
		}
		log.V(1).Info("CreateVM Result", "vm", vm)
		if vm.Error != "" {
			return patchReasonCondition(ctx, log, patchHelper, scvmmMachine, 0, nil, VmCreated, VmFailedReason, "Failed to create vm: %s", vm.Error)
		}

		log.V(1).Info("Fill in status")
		scvmmMachine.Spec.VMName = vmName
		if vm.VMId != "" {
			scvmmMachine.Spec.ProviderID = "scvmm://" + vm.VMId
		}
		scvmmMachine.Status.Ready = false
		scvmmMachine.Status.VMStatus = vm.Status
		scvmmMachine.Status.BiosGuid = vm.BiosGuid
		scvmmMachine.Status.CreationTime = vm.CreationTime
		scvmmMachine.Status.ModifiedTime = vm.ModifiedTime
		return patchReasonCondition(ctx, log, patchHelper, scvmmMachine, 10, nil, VmCreated, VmCreatingReason, "")
	}
	conditions.MarkTrue(scvmmMachine, VmCreated)

	log.V(1).Info("Machine is there, fill in status")
	if vm.VMId != "" {
		scvmmMachine.Spec.ProviderID = "scvmm://" + vm.VMId
	}
	scvmmMachine.Status.Ready = (vm.Status == "Running")
	scvmmMachine.Status.VMStatus = vm.Status
	scvmmMachine.Status.BiosGuid = vm.BiosGuid
	scvmmMachine.Status.CreationTime = vm.CreationTime
	scvmmMachine.Status.ModifiedTime = vm.ModifiedTime

	if vm.Status == "PowerOff" {
		log.V(1).Info("Call AddVMSpec")
		newspec, err := sendWinrmSpecCommand(log, cmd, "AddVMSpec", scvmmMachine)
		if err != nil {
			return patchReasonCondition(ctx, log, patchHelper, scvmmMachine, 0, err, VmCreated, VmFailedReason, "Failed calling add spec function")
		}
		log.V(1).Info("AddVMSpec result", "newspec", newspec)
		if newspec.Error != "" {
			return patchReasonCondition(ctx, log, patchHelper, scvmmMachine, 60, nil, VmCreated, VmFailedReason, "Failed calling add spec function: "+newspec.Message)
		}
		if newspec.CopyNonZeroTo(&scvmmMachine.Spec) {
			if perr := patchScvmmMachine(ctx, patchHelper, scvmmMachine); perr != nil {
				log.Error(perr, "Failed to patch scvmmMachine", "scvmmmachine", scvmmMachine)
				return ctrl.Result{}, err
			}
		}
		spec := scvmmMachine.Spec
		doexpand := false
		for i, d := range spec.Disks {
			if d.Size != nil && vm.VirtualDisks[i].MaximumSize < (d.Size.Value()-1024*1024) { // For rounding errors
				doexpand = true
			}
		}
		if doexpand {
			log.V(1).Info("Call ResizeVMDisks")
			diskjson, err := makeDisksJSON(spec.Disks)
			if err != nil {
				return patchReasonCondition(ctx, log, patchHelper, scvmmMachine, 0, err, VmCreated, VmFailedReason, "Failed to expand disks")
			}
			vm, err = sendWinrmCommand(log, cmd, "ExpandVMDisks -VMName '%s' -Disks '%s'",
				escapeSingleQuotes(scvmmMachine.Spec.VMName),
				escapeSingleQuotes(string(diskjson)))
			if err != nil {
				return patchReasonCondition(ctx, log, patchHelper, scvmmMachine, 0, err, VmCreated, VmFailedReason, "Failed to expand disks")
			}
			log.V(1).Info("ExpandVMDisks Result", "vm", vm)
			if vm.Error != "" {
				return patchReasonCondition(ctx, log, patchHelper, scvmmMachine, 0, nil, VmCreated, VmFailedReason, "Failed to create vm: %s", vm.Error)
			}
			scvmmMachine.Status.Ready = false
			scvmmMachine.Status.VMStatus = vm.Status
			scvmmMachine.Status.BiosGuid = vm.BiosGuid
			scvmmMachine.Status.CreationTime = vm.CreationTime
			scvmmMachine.Status.ModifiedTime = vm.ModifiedTime
			return patchReasonCondition(ctx, log, patchHelper, scvmmMachine, 10, nil, VmCreated, VmUpdatingReason, "")
		}

		var bootstrapData, metaData, networkConfig []byte
		if machine != nil {
			if machine.Spec.Bootstrap.DataSecretName == nil {
				if !util.IsControlPlaneMachine(machine) && !conditions.IsTrue(cluster, clusterv1.ControlPlaneInitializedCondition) {
					log.Info("Waiting for the control plane to be initialized")
					return patchReasonCondition(ctx, log, patchHelper, scvmmMachine, 0, nil, VmCreated, WaitingForControlPlaneAvailableReason, "")
				}
				log.Info("Waiting for the Bootstrap provider controller to set bootstrap data")
				return patchReasonCondition(ctx, log, patchHelper, scvmmMachine, 0, nil, VmCreated, WaitingForBootstrapDataReason, "")
			}
			log.V(1).Info("Get bootstrap data")
			bootstrapData, err = r.getBootstrapData(ctx, machine)
			if err != nil {
				r.Log.Error(err, "failed to get bootstrap data")
				return patchReasonCondition(ctx, log, patchHelper, scvmmMachine, 0, err, VmCreated, WaitingForBootstrapDataReason, "Failed to get bootstrap data")
			}
		} else if scvmmMachine.Spec.CloudInit != nil {
			if scvmmMachine.Spec.CloudInit.UserData != "" {
				bootstrapData = []byte(scvmmMachine.Spec.CloudInit.UserData)
			}
			if scvmmMachine.Spec.CloudInit.MetaData != "" {
				metaData = []byte(scvmmMachine.Spec.CloudInit.MetaData)
			}
			if scvmmMachine.Spec.CloudInit.NetworkConfig != "" {
				networkConfig = []byte(scvmmMachine.Spec.CloudInit.NetworkConfig)
			}
		}
		if metaData != nil || bootstrapData != nil || networkConfig != nil {
			log.V(1).Info("Create cloudinit")
			isoPath := provider.ScvmmLibraryISOs + "\\" + scvmmMachine.Spec.VMName + "-cloud-init.iso"
			if err := writeCloudInit(log, scvmmMachine, provider, vm.VMId, isoPath, bootstrapData, metaData, networkConfig); err != nil {
				r.Log.Error(err, "failed to create cloud init")
				return patchReasonCondition(ctx, log, patchHelper, scvmmMachine, 0, err, VmCreated, WaitingForBootstrapDataReason, "Failed to create cloud init data")
			}
			conditions.MarkFalse(scvmmMachine, VmRunning, VmStartingReason, clusterv1.ConditionSeverityInfo, "")
			if perr := patchScvmmMachine(ctx, patchHelper, scvmmMachine); perr != nil {
				log.Error(perr, "Failed to patch scvmmMachine", "scvmmmachine", scvmmMachine)
				return ctrl.Result{}, err
			}
			log.V(1).Info("Call AddIsoToVM")
			vm, err = sendWinrmCommand(log, cmd, "AddIsoToVM -VMName '%s' -ISOPath '%s'",
				escapeSingleQuotes(scvmmMachine.Spec.VMName),
				escapeSingleQuotes(isoPath))
			if err != nil {
				return ctrl.Result{}, errors.Wrap(err, "Failed to add iso to vm")
			}
			log.V(1).Info("AddIsoToVM result", "vm", vm)
		} else {
			log.V(1).Info("Call StartVM")
			vm, err = sendWinrmCommand(log, cmd, "StartVM -VMName '%s'",
				escapeSingleQuotes(scvmmMachine.Spec.VMName))
			if err != nil {
				return ctrl.Result{}, errors.Wrap(err, "Failed to start vm")
			}
			log.V(1).Info("StartVM result", "vm", vm)
		}
		scvmmMachine.Status.VMStatus = vm.Status
		if perr := patchScvmmMachine(ctx, patchHelper, scvmmMachine); perr != nil {
			log.Error(perr, "Failed to patch scvmmMachine", "scvmmmachine", scvmmMachine)
			return ctrl.Result{}, perr
		}
		log.V(1).Info("Requeue in 10 seconds")
		return ctrl.Result{RequeueAfter: time.Second * 10}, nil
	}

	// Wait for machine to get running state
	if vm.Status != "Running" {
		log.V(1).Info("Not running, Requeue in 30 seconds")
		return patchReasonCondition(ctx, log, patchHelper, scvmmMachine, 30, nil, VmRunning, VmStartingReason, "")
	}
	log.V(1).Info("Running, set status true")
	scvmmMachine.Status.Ready = true
	if vm.IPv4Addresses != nil {
		scvmmMachine.Status.Addresses = make([]clusterv1.MachineAddress, len(vm.IPv4Addresses))
		for i := range vm.IPv4Addresses {
			scvmmMachine.Status.Addresses[i] = clusterv1.MachineAddress{
				Type:    clusterv1.MachineInternalIP,
				Address: vm.IPv4Addresses[i],
			}
		}
	}
	if vm.Hostname != "" {
		scvmmMachine.Status.Hostname = vm.Hostname
	}
	conditions.MarkTrue(scvmmMachine, VmRunning)
	if perr := patchScvmmMachine(ctx, patchHelper, scvmmMachine); perr != nil {
		log.Error(perr, "Failed to patch scvmmMachine", "scvmmmachine", scvmmMachine)
		return ctrl.Result{}, perr
	}
	if vm.IPv4Addresses == nil || vm.Hostname == "" {
		log.V(1).Info("Call ReadVM")
		vm, err = sendWinrmCommand(log, cmd, "ReadVM -VMName '%s'",
			escapeSingleQuotes(scvmmMachine.Spec.VMName))
		if err != nil {
			return ctrl.Result{}, errors.Wrap(err, "Failed to read vm")
		}
		log.V(1).Info("ReadVM result", "vm", vm)
		log.Info("Reading vm IP addresses, reschedule after 60 seconds")
		return ctrl.Result{RequeueAfter: time.Second * 60}, nil
	}
	log.V(1).Info("Done")
	return ctrl.Result{}, nil
}

type VmDiskElem struct {
	SizeMB  int64  `json:"sizeMB"`
	VHDisk  string `json:"vhDisk,omitempty"`
	Dynamic bool   `json:"dynamic"`
}

func makeDisksJSON(disks []infrav1.VmDisk) ([]byte, error) {
	diskarr := make([]VmDiskElem, len(disks))
	for i, d := range disks {
		if d.Size == nil {
			diskarr[i].SizeMB = 0
		} else {
			diskarr[i].SizeMB = d.Size.Value() / 1024 / 1024
		}
		diskarr[i].VHDisk = d.VHDisk
		diskarr[i].Dynamic = d.Dynamic
	}
	return json.Marshal(diskarr)
}

func (r *ScvmmMachineReconciler) reconcileDelete(ctx context.Context, patchHelper *patch.Helper, provider *infrav1.ScvmmProviderSpec, scvmmMachine *infrav1.ScvmmMachine) (ctrl.Result, error) {
	log := r.Log.WithValues("scvmmmachine", scvmmMachine.Name)

	log.V(1).Info("Do delete reconciliation")
	// If there's no finalizer do nothing
	if !controllerutil.ContainsFinalizer(scvmmMachine, MachineFinalizer) {
		return ctrl.Result{}, nil
	}
	if scvmmMachine.Spec.VMName == "" {
		log.V(1).Info("Machine has no vmname set, remove finalizer")
		controllerutil.RemoveFinalizer(scvmmMachine, MachineFinalizer)
		if perr := patchScvmmMachine(ctx, patchHelper, scvmmMachine); perr != nil {
			log.Error(perr, "Failed to patch scvmmMachine", "scvmmmachine", scvmmMachine)
			return ctrl.Result{}, perr
		}
		return ctrl.Result{}, nil
	}
	log.V(1).Info("Set created to false, doing deletion")
	conditions.MarkFalse(scvmmMachine, VmCreated, VmDeletingReason, clusterv1.ConditionSeverityInfo, "")
	if err := patchScvmmMachine(ctx, patchHelper, scvmmMachine); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to patch ScvmmMachine")
	}

	log.Info("Doing removal of ScvmmMachine")
	cmd, err := createWinrmCmd(provider, log)
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "Winrm")
	}
	defer cmd.Close()

	log.V(1).Info("Call RemoveVM")
	vm, err := sendWinrmCommand(log, cmd, "RemoveVM -VMName '%s'",
		escapeSingleQuotes(scvmmMachine.Spec.VMName))
	if err != nil {
		return patchReasonCondition(ctx, log, patchHelper, scvmmMachine, 0, err, VmCreated, VmFailedReason, "Failed to delete VM")
	}
	log.V(1).Info("RemoveVM Result", "vm", vm)
	if vm.Error != "" {
		return patchReasonCondition(ctx, log, patchHelper, scvmmMachine, 60, nil, VmCreated, VmFailedReason, "Failed to delete VM: %s", vm.Error)
	}
	if vm.Message == "Removed" {
		adspec := scvmmMachine.Spec.ActiveDirectory
		if adspec != nil {
			log.V(1).Info("Call RemoveADComputer")
			vm, err = sendWinrmCommand(log, cmd, "RemoveADComputer -Name '%s' -OUPath '%s' -DomainController '%s'",
				escapeSingleQuotes(scvmmMachine.Spec.VMName),
				escapeSingleQuotes(adspec.OUPath),
				escapeSingleQuotes(adspec.DomainController))
			if err != nil {
				return patchReasonCondition(ctx, log, patchHelper, scvmmMachine, 0, err, VmCreated, VmFailedReason, "Failed to remove AD entry")
			}
			log.V(1).Info("RemoveADComputer Result", "vm", vm)
			if vm.Error != "" {
				return patchReasonCondition(ctx, log, patchHelper, scvmmMachine, 60, nil, VmCreated, VmFailedReason, "Failed to remove AD entry: %s", vm.Error)
			}
		}
		log.V(1).Info("Machine is removed, remove finalizer")
		controllerutil.RemoveFinalizer(scvmmMachine, MachineFinalizer)
		if perr := patchScvmmMachine(ctx, patchHelper, scvmmMachine); perr != nil {
			log.Error(perr, "Failed to patch scvmmMachine", "scvmmmachine", scvmmMachine)
			return ctrl.Result{}, perr
		}
		return ctrl.Result{}, nil
	} else {
		log.V(1).Info("Set status")
		scvmmMachine.Status.VMStatus = vm.Status
		scvmmMachine.Status.CreationTime = vm.CreationTime
		scvmmMachine.Status.ModifiedTime = vm.ModifiedTime
		log.V(1).Info("Requeue after 30 seconds")
		return patchReasonCondition(ctx, log, patchHelper, scvmmMachine, 30, err, VmCreated, VmDeletingReason, "")
	}
}

func patchReasonCondition(ctx context.Context, log logr.Logger, patchHelper *patch.Helper, scvmmMachine *infrav1.ScvmmMachine, requeue int, err error, condition clusterv1.ConditionType, reason string, message string, messageargs ...interface{}) (ctrl.Result, error) {
	scvmmMachine.Status.Ready = false
	if err != nil {
		conditions.MarkFalse(scvmmMachine, condition, reason, clusterv1.ConditionSeverityError, message, messageargs...)
	} else {
		conditions.MarkFalse(scvmmMachine, condition, reason, clusterv1.ConditionSeverityInfo, message, messageargs...)
	}
	if perr := patchScvmmMachine(ctx, patchHelper, scvmmMachine); perr != nil {
		log.Error(perr, "Failed to patch scvmmMachine", "scvmmmachine", scvmmMachine)
	}
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, reason)
	}
	if requeue != 0 {
		// This is absolutely horridly stupid.  You can't multiply a Duration with an integer,
		// so you have to cast it to a "duration" which is not actually a duration as such
		// but just a scalar masquerading as a Duration to make it work.
		//
		// (If it had been done properly, you should not have been able to multiply Duration*Duration,
		//  but only Duration*int or v.v., but I guess that's too difficult gor the go devs...)
		return ctrl.Result{RequeueAfter: time.Second * time.Duration(requeue)}, nil
	}
	return ctrl.Result{}, nil
}

func patchScvmmMachine(ctx context.Context, patchHelper *patch.Helper, scvmmMachine *infrav1.ScvmmMachine) error {
	// Always update the readyCondition by summarizing the state of other conditions.
	// A step counter is added to represent progress during the provisioning process (instead we are hiding the step counter during the deletion process).
	conditions.SetSummary(scvmmMachine,
		conditions.WithConditions(
			VmCreated,
			VmRunning,
		),
		conditions.WithStepCounterIf(scvmmMachine.ObjectMeta.DeletionTimestamp.IsZero()),
	)

	// Patch the object, ignoring conflicts on the conditions owned by this controller.
	return patchHelper.Patch(
		ctx,
		scvmmMachine,
		patch.WithOwnedConditions{Conditions: []clusterv1.ConditionType{
			clusterv1.ReadyCondition,
			VmCreated,
			VmRunning,
		}},
	)
}

func (r *ScvmmMachineReconciler) getBootstrapData(ctx context.Context, machine *clusterv1.Machine) ([]byte, error) {
	if machine.Spec.Bootstrap.DataSecretName == nil {
		return nil, errors.New("error retrieving bootstrap data: linked Machine's bootstrap.dataSecretName is nil")
	}

	s := &corev1.Secret{}
	key := client.ObjectKey{Namespace: machine.GetNamespace(), Name: *machine.Spec.Bootstrap.DataSecretName}
	if err := r.Client.Get(ctx, key, s); err != nil {
		return nil, errors.Wrapf(err, "failed to retrieve bootstrap data secret for ScvmmMachine %s/%s", machine.GetNamespace(), machine.GetName())
	}

	value, ok := s.Data["value"]
	if !ok {
		return nil, errors.New("error retrieving bootstrap data: secret value key is missing")
	}

	return value, nil
}

// ScvmmClusterToScvmmMachines is a handler.ToRequestsFunc to be used to enqeue
// requests for reconciliation of ScvmmMachines.
func (r *ScvmmMachineReconciler) ScvmmClusterToScvmmMachines(o client.Object) []ctrl.Request {
	result := []ctrl.Request{}
	c, ok := o.(*infrav1.ScvmmCluster)
	if !ok {
		r.Log.Error(errors.Errorf("expected a ScvmmCluster but got a %T", o), "failed to get ScvmmMachine for ScvmmCluster")
		return nil
	}
	log := r.Log.WithValues("ScvmmCluster", c.Name, "Namespace", c.Namespace)

	cluster, err := util.GetOwnerCluster(context.TODO(), r.Client, c.ObjectMeta)
	switch {
	case apierrors.IsNotFound(errors.Cause(err)) || cluster == nil:
		return result
	case err != nil:
		log.Error(err, "failed to get owning cluster")
		return result
	}

	labels := map[string]string{clusterv1.ClusterLabelName: cluster.Name}
	machineList := &clusterv1.MachineList{}
	if err := r.Client.List(context.TODO(), machineList, client.InNamespace(c.Namespace), client.MatchingLabels(labels)); err != nil {
		log.Error(err, "failed to list ScvmmMachines")
		return nil
	}
	for _, m := range machineList.Items {
		if m.Spec.InfrastructureRef.Name == "" {
			continue
		}
		name := client.ObjectKey{Namespace: m.Namespace, Name: m.Name}
		result = append(result, ctrl.Request{NamespacedName: name})
	}

	return result
}

func (r *ScvmmMachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	extraDebug := os.Getenv("EXTRA_DEBUG")
	if extraDebug != "" {
		ExtraDebug = true
	}

	clusterToScvmmMachines, err := util.ClusterToObjectsMapper(mgr.GetClient(), &infrav1.ScvmmMachineList{}, mgr.GetScheme())
	if err != nil {
		return err
	}
	c, err := ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.ScvmmMachine{}).
		WithEventFilter(predicates.ResourceNotPaused(r.Log)).
		Watches(
			&source.Kind{Type: &clusterv1.Machine{}},
			handler.EnqueueRequestsFromMapFunc(util.MachineToInfrastructureMapFunc(infrav1.GroupVersion.WithKind("ScvmmMachine"))),
		).
		Watches(
			&source.Kind{Type: &infrav1.ScvmmCluster{}},
			handler.EnqueueRequestsFromMapFunc(r.ScvmmClusterToScvmmMachines),
		).
		Build(r)
	if err != nil {
		return err
	}
	return c.Watch(
		&source.Kind{Type: &clusterv1.Cluster{}},
		handler.EnqueueRequestsFromMapFunc(clusterToScvmmMachines),
		predicates.ClusterUnpausedAndInfrastructureReady(r.Log),
	)
}
