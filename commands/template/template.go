package template

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	logy "github.com/apex/log"
	"github.com/briandowns/spinner"
	"github.com/netzkern/butler/config"
	"github.com/pinzolo/casee"
	survey "gopkg.in/AlecAivazis/survey.v1"
	git "gopkg.in/src-d/go-git.v4"
)

const (
	startDelim = "butler{"
	endDelim   = "}"
)

type (
	// ProjectData contains all project data
	ProjectData struct {
		Name        string
		Path        string
		Template    string
		Description string
	}
	// Templating command
	Templating struct {
		Templates    []config.Template
		Variables    map[string]string
		configName   string
		excludedDirs map[string]struct{}
		excludedExts map[string]struct{}
		ch           chan func()
		wg           sync.WaitGroup
		surveyResult map[string]interface{}
		project      *ProjectData
	}
)

// Option function.
type Option func(*Templating)

// New with the given options.
func New(options ...Option) *Templating {
	v := &Templating{
		excludedDirs: toMap(ExcludedDirs),
		excludedExts: toMap(BinaryFileExt),
		ch:           make(chan func()),
	}

	for _, o := range options {
		o(v)
	}

	return v
}

// WithVariables option.
func WithVariables(s map[string]string) Option {
	return func(v *Templating) {
		v.Variables = s
	}
}

// SetConfigName option.
func SetConfigName(s string) Option {
	return func(v *Templating) {
		v.configName = s
	}
}

// WithTemplates option.
func WithTemplates(s []config.Template) Option {
	return func(v *Templating) {
		v.Templates = s
	}
}

// cloneRepo clone a repo to the dst
func (t *Templating) cloneRepo(repoURL string, dest string) error {
	_, err := git.PlainClone(dest, false, &git.CloneOptions{
		URL: repoURL,
	})

	if err == git.ErrRepositoryAlreadyExists {
		return fmt.Errorf("respository already exists. Remove '%s' directory", dest)
	}

	return err
}

// getTemplateByName returns the template by name
func (t *Templating) getTemplateByName(name string) *config.Template {
	for _, tpl := range t.Templates {
		if tpl.Name == name {
			return &tpl
		}
	}

	return nil
}

// getTemplateOptions return an array with all template names
func (t *Templating) getTemplateOptions() []string {
	tpls := []string{}

	for _, tpl := range t.Templates {
		tpls = append(tpls, tpl.Name)
	}

	sort.Strings(tpls)

	return tpls
}

// GetQuestions return all required prompts
func (t *Templating) GetQuestions() []*survey.Question {
	qs := []*survey.Question{
		{
			Name:     "Template",
			Validate: survey.Required,
			Prompt: &survey.Select{
				Message: "What system are you using?",
				Options: t.getTemplateOptions(),
				Help:    "You can add additional templates in your config",
			},
		},
		{
			Name: "Name",
			Prompt: &survey.Input{
				Message: "What is the project name?",
				Help:    "Allowed character 0-9, A-Z, _-",
			},
			Validate: survey.Required,
		},
		{
			Name: "Description",
			Prompt: &survey.Input{
				Message: "What is the project description?",
			},
		},
		{
			Name:     "Path",
			Validate: survey.Required,
			Prompt: &survey.Input{
				Message: "What is the destination?",
				Default: "src",
				Help:    "The place of your new project",
			},
		},
	}

	return qs
}

// Skip returns an error when a directory should be skipped or true with a file
func (t *Templating) Skip(path string, info os.FileInfo) (bool, error) {
	// ignore hidden dirs and files
	if strings.HasPrefix(info.Name(), ".") {
		if info.IsDir() {
			return false, filepath.SkipDir
		}
		return true, nil
	}

	// skip blacklisted directories
	if info.IsDir() {
		_, ok := t.excludedDirs[info.Name()]
		if ok {
			return false, filepath.SkipDir
		}
	}

	// skip blacklisted extensions
	if !info.IsDir() {
		_, ok := t.excludedExts[filepath.Ext("."+info.Name())]
		if ok {
			return true, nil
		}
	}

	return false, nil
}

func (t *Templating) startCommandSurvey() error {
	var project = &ProjectData{}

	// start command prompts
	err := survey.Ask(t.GetQuestions(), project)
	if err != nil {
		return err
	}

	t.project = project

	return nil
}

func (t *Templating) startTemplateSurvey(path string) error {
	surveyResults := make(map[string]interface{})
	surveys, err := ReadSurveyConfig(path)
	if err == nil {
		questions, err := BuildSurveys(surveys)
		if err != nil {
			logy.WithError(err).Error("build surveys")
			return err
		}

		err = survey.Ask(questions, &surveyResults)

		if err != nil {
			logy.WithError(err).Error("start survey")
			return err
		}
	}

	t.surveyResult = surveyResults

	logy.Debugf("Survey results %+v", surveyResults)

	return nil
}

// Run the command
func (t *Templating) Run() error {
	err := t.startCommandSurvey()
	if err != nil {
		return err
	}

	startTime := time.Now()

	tpl := t.getTemplateByName(t.project.Template)

	// clone repository
	if tpl != nil {
		s := spinner.New(spinner.CharSets[9], 100*time.Millisecond)
		s.Suffix = "Cloning repository..."
		s.FinalMSG = "Repository cloned!\n"
		s.Start()
		err := t.cloneRepo(tpl.Url, t.project.Path)
		s.Stop()
		if err != nil {
			return err
		}
	} else {
		return fmt.Errorf("template %s could not be found", t.project.Template)
	}

	surveyFile := path.Join(t.project.Path, t.configName)
	ctx := logy.WithFields(logy.Fields{
		"path": surveyFile,
	})

	err = t.startTemplateSurvey(surveyFile)
	if err != nil {
		return err
	}

	// spinner progress
	spinner := spinner.New(spinner.CharSets[9], 100*time.Millisecond)
	spinner.Suffix = "Processing templates..."
	spinner.FinalMSG = "Templates proceed!\n"
	spinner.Start()

	// start multiple routines
	t.startN(runtime.NumCPU())

	// close sync.WaitGroup and spinner when finished
	defer func() {
		t.stop()
		spinner.Stop()
		fmt.Printf("\nTotal: %s sec \n", strconv.FormatFloat(time.Since(startTime).Seconds(), 'f', 2, 64))
	}()

	var templateData = struct {
		Project *ProjectData
		Date    string
		Year    int
		Vars    map[string]string
	}{
		t.project,
		time.Now().Format(time.RFC3339),
		time.Now().Year(),
		t.Variables,
	}

	utilFuncMap := template.FuncMap{
		"toCamelCase":  casee.ToCamelCase,
		"toPascalCase": casee.ToPascalCase,
		"toSnakeCase":  casee.ToSnakeCase,
		"join":         strings.Join,
		"getSurveyResult": func(key string) interface{} {
			val, ok := t.surveyResult[key]
			if ok {
				v, ok := val.(string)
				if ok {
					return v
				}
				return val
			}
			fmt.Printf("%+v, %v \n", val, ok)
			ctx.Errorf("map access with key '%s' failed", key)

			return val
		},
	}

	renamings := make(map[string]string)

	// iterate through all directorys
	walkDirErr := filepath.Walk(
		t.project.Path,
		func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}

			// preserve files from processing
			if !info.IsDir() {
				return nil
			}

			skipFile, skipDirErr := t.Skip(path, info)
			if skipFile {
				return nil
			}
			if skipDirErr != nil {
				return skipDirErr
			}

			ctx := logy.WithFields(logy.Fields{
				"path": path,
				"size": info.Size(),
				"dir":  info.IsDir(),
			})

			// template directorys
			t.ch <- func() {

				defer func() {
					if r := recover(); r != nil {
						ctx.Error("directory templating error")
					}
				}()

				// Template directory
				tplDir, err := template.New(path).
					Delims(startDelim, endDelim).
					Funcs(utilFuncMap).
					Parse(info.Name())

				if err != nil {
					ctx.WithError(err).Error("create template for directory")
				}

				var dirNameBuffer bytes.Buffer
				err = tplDir.Execute(&dirNameBuffer, templateData)
				if err != nil {
					ctx.WithError(err).Error("execute template for directory")
				}

				newDirectory := dirNameBuffer.String()
				newPath := filepath.Join(filepath.Dir(path), newDirectory)

				if path != newPath {
					renamings[path] = newPath
				}
			}

			return nil
		})

	if walkDirErr != nil {
		return walkDirErr
	}

	// rename and remove dirs
	for oldPath, newPath := range renamings {
		os.Rename(oldPath, newPath)
		os.RemoveAll(oldPath)
	}

	// iterate through all files
	walkErr := filepath.Walk(t.project.Path,
		func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}

			skipFile, skipDirErr := t.Skip(path, info)
			if skipFile {
				ctx.Debug("skip file")
				return nil
			}
			if skipDirErr != nil {
				ctx.Debug("skip directory")
				return skipDirErr
			}

			// preserve directorys from processing
			if info.IsDir() {
				return nil
			}

			ctx := logy.WithFields(logy.Fields{
				"path": path,
				"size": info.Size(),
				"dir":  info.IsDir(),
			})

			// template file
			t.ch <- func() {

				defer func() {
					if r := recover(); r != nil {
						ctx.Error("templating error")
					}
				}()

				// Template filename
				tplFilename, err := template.New(path).
					Delims(startDelim, endDelim).
					Funcs(utilFuncMap).
					Parse(info.Name())

				if err != nil {
					ctx.WithError(err).Error("create template for filename")
					return
				}

				var filenameBuffer bytes.Buffer
				err = tplFilename.Execute(&filenameBuffer, templateData)
				if err != nil {
					ctx.WithError(err).Error("execute template for filename")
					return
				}

				newFilename := filenameBuffer.String()
				newPath := filepath.Join(filepath.Dir(path), newFilename)
				dat, err := ioutil.ReadFile(path)

				if err != nil {
					ctx.WithError(err).Error("read")
					return
				}

				// Template file content
				tmpl, err := template.New(newPath).
					Delims(startDelim, endDelim).
					Funcs(utilFuncMap).
					Parse(string(dat))

				if err != nil {
					ctx.WithError(err).Error("parse")
					return
				}

				f, err := os.Create(newPath)

				if err != nil {
					ctx.WithError(err).Error("create")
					return
				}

				defer f.Close()

				err = tmpl.Execute(f, templateData)

				if err != nil {
					ctx.WithError(err).Error("template")
					return
				}

				// remove old file when the name was changed
				if path != newPath {
					ctx.Debug("filename changed")
					os.Remove(path)
				}
			}

			return nil
		})

	if walkErr != nil {
		return walkErr
	}

	return nil
}

// startN starts n loops.
func (t *Templating) startN(n int) {
	for i := 0; i < n; i++ {
		t.wg.Add(1)
		go t.start()
	}
}

// start loop.
func (t *Templating) start() {
	defer t.wg.Done()
	for fn := range t.ch {
		fn()
	}
}

// stop loop.
func (t *Templating) stop() {
	close(t.ch)
	t.wg.Wait()
}

// toMap returns a map from slice.
func toMap(s []string) map[string]struct{} {
	m := make(map[string]struct{})
	for _, v := range s {
		m[v] = struct{}{}
	}
	return m
}
