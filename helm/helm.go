package helm

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/romana/rlog"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kblabels "k8s.io/apimachinery/pkg/labels"

	"github.com/flant/antiopa/executor"
	"github.com/flant/antiopa/kube"
	"github.com/flant/antiopa/utils"
)

type HelmClient interface {
	TillerNamespace() string
	CommandEnv() []string
	Cmd(args ...string) (string, string, error)
	DeleteSingleFailedRevision(releaseName string) error
	DeleteOldFailedRevisions(releaseName string) error
	LastReleaseStatus(releaseName string) (string, string, error)
	UpgradeRelease(releaseName string, chart string, valuesPaths []string, setValues []string, namespace string) error
	GetReleaseValues(releaseName string) (utils.Values, error)
	DeleteRelease(releaseName string) error
	ListReleases(labelSelector map[string]string) ([]string, error)
	ListReleasesNames(labelSelector map[string]string) ([]string, error)
	IsReleaseExists(releaseName string) (bool, error)
}

type CliHelm struct {
	tillerNamespace string
}

// InitHelm запускает установку tiller-a.
func Init(tillerNamespace string) (HelmClient, error) {
	rlog.Info("Helm: run helm init")

	helm := &CliHelm{tillerNamespace: tillerNamespace}

	err := helm.InitTiller()
	if err != nil {
		return nil, err
	}

	stdout, stderr, err := helm.Cmd("version")
	if err != nil {
		return nil, fmt.Errorf("unable to get helm version: %v\n%v %v", err, stdout, stderr)
	}
	rlog.Infof("Helm: helm version:\n%v %v", stdout, stderr)

	rlog.Info("Helm: successfully initialized")

	return helm, nil
}

func (helm *CliHelm) InitTiller() error {
	antiopaDeploy, err := kube.KubernetesClient.AppsV1beta1().Deployments(kube.KubernetesAntiopaNamespace).Get(kube.AntiopaDeploymentName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("cannot fetch antiopa deployment to gather settings for tiller deployment: %s", err)
	}

	cmd := make([]string, 0)
	cmd = append(cmd,
		"init",
		"--service-account", "antiopa",
		"--upgrade", "--wait", "--skip-refresh",
	)

	nodeSelectors := make([]string, 0)
	for k, v := range antiopaDeploy.Spec.Template.Spec.NodeSelector {
		nodeSelectors = append(nodeSelectors, fmt.Sprintf("%s=%s", k, v))
	}
	if len(nodeSelectors) > 0 {
		cmd = append(cmd, fmt.Sprintf("--node-selectors=%s", strings.Join(nodeSelectors, ",")))
	}

	override := make([]string, 0)
	for i, spec := range antiopaDeploy.Spec.Template.Spec.Tolerations {
		override = append(override, fmt.Sprintf("spec.template.spec.tolerations[%d].key=%s", i, spec.Key))
		override = append(override, fmt.Sprintf("spec.template.spec.tolerations[%d].operator=%s", i, spec.Operator))
		override = append(override, fmt.Sprintf("spec.template.spec.tolerations[%d].value=%s", i, spec.Value))
		override = append(override, fmt.Sprintf("spec.template.spec.tolerations[%d].effect=%s", i, spec.Effect))

		if spec.TolerationSeconds != nil {
			override = append(override, fmt.Sprintf("spec.template.spec.tolerations[%d].tolerationSeconds=%d", i, *spec.TolerationSeconds))
		}
	}
	if len(override) > 0 {
		cmd = append(cmd, fmt.Sprintf("--override=%s", strings.Join(override, ",")))
	}

	stdout, stderr, err := helm.Cmd(cmd...)
	if err != nil {
		return fmt.Errorf("%s\n%s\n%s", err, stdout, stderr)
	}
	rlog.Infof("Helm: tiller initialization done: %v %v", stdout, stderr)

	return nil
}

func (helm *CliHelm) TillerNamespace() string {
	return helm.tillerNamespace
}

func (helm *CliHelm) CommandEnv() []string {
	res := make([]string, 0)
	res = append(res, fmt.Sprintf("TILLER_NAMESPACE=%s", helm.TillerNamespace()))
	return res
}

// Запускает helm с переданными аргументами.
// Перед запуском устанавливает переменную среды TILLER_NAMESPACE,
// чтобы antiopa работала со своим tiller-ом.
func (helm *CliHelm) Cmd(args ...string) (stdout string, stderr string, err error) {
	binPath := "/usr/local/bin/helm"
	cmd := exec.Command(binPath, args...)
	cmd.Env = append(os.Environ(), helm.CommandEnv()...)

	var stdoutBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	err = executor.Run(cmd, true)
	stdout = strings.TrimSpace(stdoutBuf.String())
	stderr = strings.TrimSpace(stderrBuf.String())

	return
}

func (helm *CliHelm) DeleteSingleFailedRevision(releaseName string) (err error) {
	revision, status, err := helm.LastReleaseStatus(releaseName)
	if err != nil {
		if revision == "0" {
			// revision 0 is not an error. just skip deletion.
			rlog.Debugf("helm release '%s': Release not found, no cleanup required.", releaseName)
			return nil
		}
		rlog.Errorf("helm release '%s': got error from LastReleaseStatus: %s", releaseName, err)
		return err
	}

	if revision == "1" && status == "FAILED" {
		// delete and purge!
		err = helm.DeleteRelease(releaseName)
		if err != nil {
			rlog.Errorf("helm release '%s': cleanup of failed revision got error: %v", releaseName, err)
			return err
		}
		rlog.Infof("helm release '%s': cleanup of failed revision succeeded", releaseName)
	} else {
		// No interest of revisions older than 1
		rlog.Debugf("helm release '%s': has revision '%s' with status %s", releaseName, revision, status)
	}

	return
}

func (helm *CliHelm) DeleteOldFailedRevisions(releaseName string) error {
	cmNames, err := helm.ListReleases(map[string]string{"STATUS": "FAILED", "NAME": releaseName})
	if err != nil {
		return err
	}

	rlog.Debugf("helm release '%s': found ConfigMaps: %v", cmNames)

	var releaseCmNamePattern = regexp.MustCompile(`^(.*).v([0-9]+)$`)

	revisions := make([]int, 0)
	for _, cmName := range cmNames {
		matchRes := releaseCmNamePattern.FindStringSubmatch(cmName)
		if matchRes != nil {
			revision, err := strconv.Atoi(matchRes[2])
			if err != nil {
				continue
			}
			revisions = append(revisions, revision)
		}
	}
	sort.Ints(revisions)

	// Do not remove last FAILED revision
	if len(revisions) > 0 {
		revisions = revisions[:len(revisions)-1]
	}

	for _, revision := range revisions {
		cmName := fmt.Sprintf("%s.v%d", releaseName, revision)
		rlog.Infof("helm release '%s': delete old FAILED revision cm/%s", releaseName, cmName)

		err := kube.KubernetesClient.CoreV1().
			ConfigMaps(kube.KubernetesAntiopaNamespace).
			Delete(cmName, &metav1.DeleteOptions{})

		if err != nil {
			return err
		}
	}

	return nil
}

// Get last known revision and status
// helm history output:
// REVISION	UPDATED                 	STATUS    	CHART                 	DESCRIPTION
// 1        Fri Jul 14 18:25:00 2017	SUPERSEDED	symfony-demo-0.1.0    	Install complete
func (helm *CliHelm) LastReleaseStatus(releaseName string) (revision string, status string, err error) {
	stdout, stderr, err := helm.Cmd("history", releaseName, "--max", "1")

	if err != nil {
		errLine := strings.Split(stderr, "\n")[0]
		if strings.Contains(errLine, "Error:") && strings.Contains(errLine, "not found") {
			// Bad module name or no releases installed
			err = fmt.Errorf("release '%s' not found\n%v %v", releaseName, stdout, stderr)
			revision = "0"
			return
		}

		err = fmt.Errorf("cannot get history for release '%s'\n%v %v", releaseName, stdout, stderr)
		return
	}

	historyLines := strings.Split(stdout, "\n")
	lastLine := historyLines[len(historyLines)-1]
	fields := strings.SplitN(lastLine, "\t", 5) //regexp.MustCompile("\\t").Split(lastLine, 5)
	revision = strings.TrimSpace(fields[0])
	status = strings.TrimSpace(fields[2])
	return
}

func (helm *CliHelm) UpgradeRelease(releaseName string, chart string, valuesPaths []string, setValues []string, namespace string) error {
	args := make([]string, 0)
	args = append(args, "upgrade")
	args = append(args, "--install")
	args = append(args, releaseName)
	args = append(args, chart)

	if namespace != "" {
		args = append(args, "--namespace")
		args = append(args, namespace)
	}

	for _, valuesPath := range valuesPaths {
		args = append(args, "--values")
		args = append(args, valuesPath)
	}

	for _, setValue := range setValues {
		args = append(args, "--set")
		args = append(args, setValue)
	}

	rlog.Infof("Running helm upgrade for release '%s' with chart '%s' in namespace '%s' ...", releaseName, chart, namespace)
	stdout, stderr, err := helm.Cmd(args...)
	if err != nil {
		return fmt.Errorf("helm upgrade failed: %s:\n%s %s", err, stdout, stderr)
	}
	rlog.Infof("Helm upgrade for release '%s' with chart '%s' in namespace '%s' successful:\n%s\n%s", releaseName, chart, namespace, stdout, stderr)

	return nil
}

func (helm *CliHelm) GetReleaseValues(releaseName string) (utils.Values, error) {
	stdout, stderr, err := helm.Cmd("get", "values", releaseName)
	if err != nil {
		return nil, fmt.Errorf("cannot get values of helm release %s: %s\n%s %s", releaseName, err, stdout, stderr)
	}

	values, err := utils.NewValuesFromBytes([]byte(stdout))
	if err != nil {
		return nil, fmt.Errorf("cannot get values of helm release %s: %s", releaseName, err)
	}

	return values, nil
}

func (helm *CliHelm) DeleteRelease(releaseName string) (err error) {
	rlog.Debugf("helm release '%s': execute helm delete --purge", releaseName)

	stdout, stderr, err := helm.Cmd("delete", "--purge", releaseName)
	if err != nil {
		return fmt.Errorf("helm delete --purge %s invocation error: %v\n%v %v", releaseName, err, stdout, stderr)
	}

	return
}

func (helm *CliHelm) IsReleaseExists(releaseName string) (bool, error) {
	revision, _, err := helm.LastReleaseStatus(releaseName)
	if err != nil && revision == "0" {
		return false, nil
	} else if err != nil {
		return false, err
	}

	return true, nil
}

// Возвращает все известные релизы в виде строк "<имя_релиза>.v<номер_версии>"
// helm ищет ConfigMap-ы по лейблу OWNER=TILLER и получает данные о релизе из ключа "release"
// https://github.com/kubernetes/helm/blob/8981575082ea6fc2a670f81fb6ca5b560c4f36a7/pkg/storage/driver/cfgmaps.go#L88
func (helm *CliHelm) ListReleases(labelSelector map[string]string) ([]string, error) {
	labelsSet := make(kblabels.Set)
	for k, v := range labelSelector {
		labelsSet[k] = v
	}
	labelsSet["OWNER"] = "TILLER"

	cmList, err := kube.KubernetesClient.CoreV1().
		ConfigMaps(kube.KubernetesAntiopaNamespace).
		List(metav1.ListOptions{LabelSelector: labelsSet.AsSelector().String()})
	if err != nil {
		rlog.Debugf("helm: list of releases ConfigMaps failed: %s", err)
		return nil, err
	}

	releases := make([]string, 0)
	for _, cm := range cmList.Items {
		if _, has_key := cm.Data["release"]; has_key {
			releases = append(releases, cm.Name)
		}
	}

	sort.Strings(releases)

	return releases, nil
}

// Список имён релизов без суффикса ".v<номер релиза>"
func (helm *CliHelm) ListReleasesNames(labelSelector map[string]string) ([]string, error) {
	releases, err := helm.ListReleases(labelSelector)
	if err != nil {
		return []string{}, err
	}

	var releaseCmNamePattern = regexp.MustCompile(`^(.*).v[0-9]+$`)

	releasesNamesMap := map[string]bool{}
	for _, release := range releases {
		matchRes := releaseCmNamePattern.FindStringSubmatch(release)
		if matchRes != nil {
			releaseName := matchRes[1]
			releasesNamesMap[releaseName] = true
		}
	}

	releasesNames := make([]string, 0)
	for releaseName := range releasesNamesMap {
		releasesNames = append(releasesNames, releaseName)
	}

	return releasesNames, nil
}
