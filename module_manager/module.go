package module_manager

import (
	"encoding/json"
	"fmt"
	"github.com/romana/rlog"
	"gopkg.in/yaml.v2"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/flant/antiopa/helm"
	"github.com/flant/antiopa/merge_values"
	"github.com/flant/antiopa/utils"
)

type Module struct {
	Name          string
	DirectoryName string
	Path          string
}

func (m *Module) run() error {
	if err := m.cleanup(); err != nil {
		return err
	}

	moduleHooksBeforeHelm, err := GetModuleHooksInOrder(m.Name, BeforeHelm)
	if err != nil {
		return err
	}

	for _, moduleHookName := range moduleHooksBeforeHelm {
		moduleHook, err := GetModuleHook(moduleHookName)
		if err != nil {
			return err
		}

		if err := moduleHook.run(); err != nil {
			return err
		}
	}

	if err := m.exec(); err != nil {
		return err
	}

	moduleHooksAfterHelm, err := GetModuleHooksInOrder(m.Name, AfterHelm)
	if err != nil {
		return err
	}

	for _, moduleHookName := range moduleHooksAfterHelm {
		moduleHook, err := GetModuleHook(moduleHookName)
		if err != nil {
			return err
		}

		if err := moduleHook.run(); err != nil {
			return err
		}
	}

	return nil
}

func (m *Module) cleanup() error {
	chartExists, err := m.checkHelmChart()
	if !chartExists {
		if err != nil {
			rlog.Debugf("Module '%s': cleanup not needed: %s", m.Name, err)
			return nil
		}
	}

	rlog.Infof("Module '%s': running cleanup ...", m.Name)

	if err := helm.HelmDeleteSingleFailedRevision(m.generateHelmReleaseName()); err != nil {
		return err
	}

	return nil
}

func (m *Module) exec() error {
	chartExists, err := m.checkHelmChart()
	if !chartExists {
		if err != nil {
			rlog.Debugf("Module '%s': helm not needed: %s", m.Name, err)
			return nil
		}
	}

	rlog.Infof("Module '%s': running helm ...", m.Name)

	helmReleaseName := m.generateHelmReleaseName()
	valuesPath, err := m.prepareValuesPath()
	if err != nil {
		return err
	}

	err = execCommand(makeCommand(m.Path, valuesPath, "helm", []string{"upgrade", helmReleaseName, ".", "--install", "--namespace", helm.TillerNamespace, "--values", valuesPath}))
	if err != nil {
		return fmt.Errorf("module '%s': helm FAILED: %s", m.Name, err)
	}

	return nil
}

func (m *Module) setGlobalModuleConfigValues() error {
	path := filepath.Join(m.Path, "values.yaml")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}

	values, err := readValuesYamlFile(path)
	if err != nil {
		return err
	}

	globalModulesConfigValues[m.Name] = values

	return nil
}

func (m *Module) prepareValuesPath() (string, error) {
	valuesPath, err := dumpValuesYaml(fmt.Sprintf("%s.yaml", m.Name), m.values())
	if err != nil {
		return "", err
	}
	return valuesPath, nil
}

func (m *Module) checkHelmChart() (bool, error) {
	chartPath := filepath.Join(m.Path, "Chart.yaml")

	if _, err := os.Stat(chartPath); os.IsNotExist(err) {
		return false, fmt.Errorf("module `%s` chart file not found '%s'", m.Name, chartPath)
	}
	return true, nil
}

func (m *Module) generateHelmReleaseName() string {
	return m.Name
}

func (m *Module) values() map[interface{}]interface{} {
	return merge_values.MergeValues(
		globalConfigValues,
		globalModulesConfigValues[m.Name],
		kubeConfigValues,
		kubeModulesConfigValues[m.Name],
		dynamicValues,
		modulesDynamicValues[m.Name])
}

func (m *Module) isEnabled() (bool, error) {
	// moduleValues := m.values()
	// TODO check values

	enabledScriptPath := filepath.Join(m.DirectoryName, "enabled")

	_, err := os.Stat(enabledScriptPath)
	if os.IsNotExist(err) {
		return true, nil
	} else if err != nil {
		return false, err
	}

	enabledModulesFilePath, err := dumpValuesJson(filepath.Join("enabled-modules", m.Name), modulesOrder)
	if err != nil {
		return false, err
	}

	cmd := makeCommand(m.Path, "", enabledScriptPath, []string{})
	cmd.Env = append(cmd.Env, fmt.Sprintf("ENABLED_MODULES_PATH=%s", enabledModulesFilePath))
	if err := execCommand(cmd); err != nil {
		return false, err
	}

	return true, nil
}

func initModules() error {
	rlog.Debug("Init modules")

	modulesByName = make(map[string]*Module)
	modulesHooksByName = make(map[string]*ModuleHook)
	modulesHooksOrderByName = make(map[string]map[BindingType][]*ModuleHook)

	modulesDir := filepath.Join(WorkingDir, "modules")

	files, err := ioutil.ReadDir(modulesDir) // returns a list of modules sorted by filename
	if err != nil {
		return fmt.Errorf("cannot list modules directory %s: %s", modulesDir, err)
	}

	if err := setGlobalConfigValues(); err != nil {
		return err
	}

	var validModuleName = regexp.MustCompile(`^[0-9][0-9][0-9]-(.*)$`)

	badModulesDirs := make([]string, 0)

	for _, file := range files {
		if file.IsDir() {
			matchRes := validModuleName.FindStringSubmatch(file.Name())
			if matchRes != nil {
				moduleName := matchRes[1]
				modulePath := filepath.Join(modulesDir, file.Name())

				module := &Module{
					Name:          moduleName,
					DirectoryName: file.Name(),
					Path:          modulePath,
				}
				module.setGlobalModuleConfigValues()

				isEnabled, err := module.isEnabled()
				if err != nil {
					return err
				}

				if isEnabled {
					modulesByName[module.Name] = module
					modulesOrder = append(modulesOrder, module.Name)

					if err = initModuleHooks(module); err != nil {
						return err
					}
				}
			} else {
				badModulesDirs = append(badModulesDirs, filepath.Join(modulesDir, file.Name()))
			}
		}
	}

	if len(badModulesDirs) > 0 {
		return fmt.Errorf("bad module directory names, must match regex `%s`: %s", validModuleName, strings.Join(badModulesDirs, ", "))
	}

	return nil
}

func setGlobalConfigValues() (err error) {
	globalConfigValues, err = readModulesValues()
	if err != nil {
		return err
	}
	return nil
}

func readModulesValues() (map[interface{}]interface{}, error) {
	path := filepath.Join(WorkingDir, "modules", "values.yaml")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return make(map[interface{}]interface{}), nil
	}
	return readValuesYamlFile(path)
}

func getExecutableFilesPaths(dir string) ([]string, error) {
	paths := make([]string, 0)
	err := filepath.Walk(dir, func(path string, f os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if f.IsDir() {
			return nil
		}

		isExecutable := f.Mode()&0111 != 0
		if isExecutable {
			paths = append(paths, path)
		} else {
			rlog.Warnf("Ignoring non executable file %s", filepath.Join(dir, path))
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	return paths, nil
}

func readValuesYamlFile(filePath string) (map[interface{}]interface{}, error) {
	valuesYaml, err := ioutil.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %s", filePath, err)
	}

	var res map[interface{}]interface{}

	err = yaml.Unmarshal(valuesYaml, &res)
	if err != nil {
		return nil, fmt.Errorf("bad %s: %s", filePath, err)
	}

	return res, nil
}

func dumpValuesYaml(fileName string, values map[interface{}]interface{}) (string, error) {
	valuesYaml, err := yaml.Marshal(&values)
	if err != nil {
		return "", err
	}

	filePath := filepath.Join(TempDir, fileName)
	if err = dumpData(filePath, valuesYaml); err != nil {
		return "", err
	}

	return filePath, nil
}

func dumpValuesJson(fileName string, values interface{}) (string, error) {
	valuesJson, err := json.Marshal(&values)
	if err != nil {
		return "", err
	}

	filePath := filepath.Join(TempDir, fileName)
	if err = dumpData(filePath, valuesJson); err != nil {
		return "", err
	}

	return filePath, nil
}

func dumpData(filePath string, data []byte) error {
	err := ioutil.WriteFile(filePath, data, 0644)
	if err != nil {
		return err
	}
	return nil
}

func valuesToString(values map[interface{}]interface{}) string {
	valuesYaml, err := yaml.Marshal(&values)
	if err != nil {
		return fmt.Sprintf("%v", values)
	}
	return string(valuesYaml)
}

func makeCommand(dir string, valuesPath string, entrypoint string, args []string) *exec.Cmd {
	envs := make([]string, 0)
	envs = append(envs, os.Environ()...)
	envs = append(envs, helm.CommandEnv()...)
	envs = append(envs, fmt.Sprintf("VALUES_PATH=%s", valuesPath))

	return utils.MakeCommand(dir, entrypoint, args, envs)
}

func execCommand(cmd *exec.Cmd) error {
	rlog.Debugf("Executing command in %s: `%s`", cmd.Dir, strings.Join(cmd.Args, " "))
	return cmd.Run()
}