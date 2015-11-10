package main

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/mattn/go-colorable"
	"github.com/tonnerre/golang-pretty"

	"github.com/alecthomas/kingpin"
	"gopkg.in/inconshreveable/log15.v2"

	"github.com/rightscale/rsc/cm15"
	"github.com/rightscale/rsc/httpclient"
	"github.com/rightscale/rsc/log"
	"github.com/rightscale/rsc/rsapi"
)

var (
	app         = kingpin.New("right_st", "A command-line application for managing RightScripts")
	version     = app.Flag("version", "Print version").Short('v').Bool()
	debug       = app.Flag("debug", "Debug mode").Short('d').Bool()
	configFile  = app.Flag("config", "Set the config file path.").Short('c').Default(defaultConfigFile()).String()
	environment = app.Flag("environment", "Set the RightScale login environment.").Short('e').String()

	rightScript = app.Command("rightscript", "RightScript stuff")

	rightScriptList       = rightScript.Command("list", "List RightScripts")
	rightScriptListFilter = rightScriptList.Arg("filter", "Filter by name").Required().String()

	rightScriptShow           = rightScript.Command("show", "Show a single RightScript and its attachments")
	rightScriptShowNameOrHref = rightScriptShow.Arg("name_or_href", "Script Name or Href").Required().String()

	rightScriptUpload      = rightScript.Command("upload", "Upload a RightScript")
	rightScriptUploadPaths = rightScriptUpload.Arg("path", "File or directory containing script files to upload").Required().ExistingFilesOrDirs()
	rightScriptUploadForce = rightScriptUpload.Flag("force", "Force upload of file if metadata is not present").Bool()

	rightScriptDownload           = rightScript.Command("download", "Download a RightScript to a file or files")
	rightScriptDownloadNameOrHref = rightScriptDownload.Arg("name_or_href", "Script Name or Href").Required().String()
	rightScriptDownloadTo         = rightScriptDownload.Arg("path", "Download location").String()

	rightScriptScaffold      = rightScript.Command("scaffold", "Add RightScript YAML metadata comments to a file or files")
	rightScriptScaffoldPaths = rightScriptScaffold.Arg("path", "File or directory to set metadata for").Required().ExistingFilesOrDirs()

	rightScriptValidate      = rightScript.Command("validate", "Validate RightScript YAML metadata comments in a file or files")
	rightScriptValidatePaths = rightScriptValidate.Arg("path", "Path to script file or directory containing script files").Required().ExistingFilesOrDirs()
)

func main() {
	app.HelpFlag.Short('h')
	command := kingpin.MustParse(app.Parse(os.Args[1:]))

	err := readConfig(*configFile, *environment)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: Error reading config file: %s\n", filepath.Base(os.Args[0]), err)
		os.Exit(1)
	}
	client := config.environment.Client15()

	// Handle logging
	logLevel := log15.LvlInfo

	if *debug {
		log.Logger.SetHandler(
			log15.LvlFilterHandler(
				log15.LvlDebug,
				log15.StderrHandler))
		httpclient.DumpFormat = httpclient.Debug
		logLevel = log15.LvlDebug
	}
	handler := log15.LvlFilterHandler(logLevel, log15.StreamHandler(colorable.NewColorableStdout(), log15.TerminalFormat()))
	log15.Root().SetHandler(handler)
	app.Writer(os.Stdout)

	switch command {
	case rightScriptList.FullCommand():
		rightscriptLocator := client.RightScriptLocator("/api/right_scripts")
		var apiParams = rsapi.APIParams{"filter": []string{"name==" + *rightScriptListFilter}}
		fmt.Printf("Listing %s:\n", *rightScriptListFilter)
		//log15.Info("Listing", "RightScript", *rightScriptListFilter)
		rightscripts, err := rightscriptLocator.Index(apiParams)
		if err != nil {
			fatalError("%s", err.Error())
		}
		for _, rs := range rightscripts {
			rev := "HEAD"
			if rs.Revision != 0 {
				rev = fmt.Sprintf("%d", rs.Revision)
			}
			fmt.Printf("/api/right_scripts/%s %5s %s\n", rs.Id, rev, rs.Name)
		}
	case rightScriptShow.FullCommand():
		href, err := rightscriptParamToHref(*rightScriptShowNameOrHref)
		attachmentsHref := fmt.Sprintf("%s/attachments", href)
		if err != nil {
			fatalError("%s", err.Error())
		}

		rightscriptLocator := client.RightScriptLocator(href)
		attachmentsLocator := client.RightScriptAttachmentLocator(attachmentsHref)

		rightscript, err := rightscriptLocator.Show()
		if err != nil {
			fatalError("Could not find rightscript with href %s: %s", href, err.Error())
		}
		attachments, err := attachmentsLocator.Index(rsapi.APIParams{})
		if err != nil {
			fatalError("Could not find attachments with href %s: %s", attachmentsHref, err.Error())
		}
		rev := "HEAD"
		if rightscript.Revision != 0 {
			rev = fmt.Sprintf("%d", rightscript.Revision)
		}
		fmt.Printf("HREF: /api/right_scripts/%s\n", rightscript.Id)
		fmt.Printf("Revision: %5s\n", rev)
		fmt.Printf("Name: %s\n", rightscript.Name)
		fmt.Printf("Attachments (id, md5, name):\n")
		for _, a := range attachments {
			fmt.Printf("  %d %s %s\n", a.Id, a.Digest, a.Name)
		}
	case rightScriptUpload.FullCommand():
		// Pass 1, perform validations, gather up results
		scripts := []RightScript{}
		paths, err := walkPaths(rightScriptUploadPaths)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %s\n", filepath.Base(os.Args[0]), err)
			os.Exit(1)
		}

		for _, p := range paths {
			fmt.Printf("Uploading %s\n", p)
			f, err := os.Open(p)
			if err != nil {
				fatalError("Cannot open %s", p)
			}
			defer f.Close()
			metadata, err := ParseRightScriptMetadata(f)

			if metadata.Name == "" {
				if !*rightScriptUploadForce {
					fatalError("No embedded metadata for %s. Use --force to upload anyways.", p)
				}
				scriptname := path.Base(p)
				scriptext := path.Ext(scriptname)
				scriptname = strings.TrimRight(scriptname, scriptext)
				metadata.Name = scriptname
			}

			script := RightScript{"", p, metadata}
			scripts = append(scripts, script)

			// validate all attachments exist and are readable
		}

		// Pass 2, upload
		for _, script := range scripts {
			err = script.Push()
			if err != nil {
				fatalError("%s", err.Error())
			}
		}
	case rightScriptDownload.FullCommand():
		href, err := rightscriptParamToHref(*rightScriptDownloadNameOrHref)
		if err != nil {
			fatalError("%s", err.Error())
		}

		rightscriptLocator := client.RightScriptLocator(href)
		// attachmentsLocator := client.RightScriptLocator(fmt.Sprintf("%s/attachments", href))

		rightscript, err := rightscriptLocator.Show()
		if err != nil {
			fatalError("Could not find rightscript with href %s: %s", href, err.Error())
		}
		source, err := getSource(rightscriptLocator)
		if err != nil {
			fatalError("Could get soruce for rightscript with href %s: %s", href, err.Error())
		}

		// attachments, err2 := attachmentsLocator.Index(rsapi.APIParams{})
		if *rightScriptDownloadTo == "" {
			*rightScriptDownloadTo = rightscript.Name
		}
		fmt.Printf("Downloading '%s' to %s\n", rightscript.Name, *rightScriptDownloadTo)
		err = ioutil.WriteFile(*rightScriptDownloadTo, source, 0755)
		if err != nil {
			fatalError("Could not create file: %s", err.Error())
		}

	case rightScriptScaffold.FullCommand():
		paths, err := walkPaths(rightScriptScaffoldPaths)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %s\n", filepath.Base(os.Args[0]), err)
			os.Exit(1)
		}

		for _, path := range paths {
			err = AddRightScriptMetadata(path)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s: %s\n", filepath.Base(os.Args[0]), err)
				os.Exit(1)
			}
		}
	case rightScriptValidate.FullCommand():
		paths, err := walkPaths(rightScriptValidatePaths)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %s\n", filepath.Base(os.Args[0]), err)
			os.Exit(1)
		}

		err_encountered := false

		for _, path := range paths {
			err = validateRightScript(path)
			if err != nil {
				err_encountered = true
				fmt.Fprintf(os.Stderr, "%s - %s: %s\n", path, filepath.Base(os.Args[0]), err)
			}
		}
		if err_encountered {
			os.Exit(1)
		}
	}
}

func rightscriptParamToHref(param string) (string, error) {
	client := config.environment.Client15()

	rsIdMatch := regexp.MustCompile(`^\d+$`)
	rsHrefMatch := regexp.MustCompile(`^/api/right_scripts/\d+$`)
	var href string
	if rsIdMatch.Match([]byte(param)) {
		href = fmt.Sprintf("/api/right_scripts/%s", param)
	} else if rsHrefMatch.Match([]byte(param)) {
		href = param
	} else {
		rightscriptLocator := client.RightScriptLocator("/api/right_scripts")
		apiParams := rsapi.APIParams{"filter": []string{"name==" + param}}
		rightscripts, err := rightscriptLocator.Index(apiParams)
		if err != nil {
			return "", err
		}
		foundId := ""
		for _, rs := range rightscripts {
			//fmt.Printf("%#v\n", rs)
			// Recheck the name here, filter does a impartial match and we need an exact one
			// TODO, do first pass for head revisions only, second for non-heads?
			if rs.Name == param && rs.Revision == 0 {
				if foundId != "" {
					return "", fmt.Errorf("Error, matched multiple RightScripts with the same name. Don't know which one to download. Please delete one or specify an HREF to download such as /api/right_scripts/%d", rs.Id)
				} else {
					foundId = rs.Id
				}
			}
		}
		if foundId == "" {
			return "", fmt.Errorf("Found no RightScripts matching %s", param)
		}
		href = fmt.Sprintf("/api/right_scripts/%s", foundId)
	}
	return href, nil
}

// Turn a mixed array of directories and files into a linear list of files
func walkPaths(paths *[]string) ([]string, error) {
	files := []string{}
	for _, path := range *paths {
		info, err := os.Stat(path)
		if err != nil {
			return files, err
		}
		if info.IsDir() {
			err = filepath.Walk(path, func(p string, f os.FileInfo, err error) error {
				files = append(files, p)
				_, e := os.Stat(p)
				return e
			})
			if err != nil {
				return files, err
			}
		} else {
			files = append(files, path)
		}
	}
	return files, nil

}

// Crappy workaround. RSC doesn't return the body of the http request which contains
// the script source, so do the same lower level calls it does to get it.
func getSource(loc *cm15.RightScriptLocator) (respBody []byte, err error) {
	var params rsapi.APIParams
	var p rsapi.APIParams
	APIVersion := "1.5"
	client := config.environment.Client15()

	uri, err := loc.ActionPath("RightScript", "show_source")
	if err != nil {
		return respBody, err
	}
	req, err := client.BuildHTTPRequest(uri.HTTPMethod, uri.Path, APIVersion, params, p)
	if err != nil {
		return respBody, err
	}
	resp, err := client.PerformRequest(req)
	if err != nil {
		return respBody, err
	}
	defer resp.Body.Close()
	respBody, _ = ioutil.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return respBody, fmt.Errorf("invalid response %s: %s", resp.Status, string(respBody))
	}
	return respBody, nil
}

// Crappy workaround 2. the RightScriptAttachmentLocator.Create call doesn't work
// because RSCs countless concrete types screw things up. The RSC create call calls BuildHttpRequest
// with the type passed in, which is serializes to JSON. Under different code paths 
// (such as here or the command line) it passes in rsapi.APIParams instead of a fixed type of
// cm15.RightScriptAttachmentParams. BuildHTTPRequest has code to iterate over APIParams and
// turn it into a a multipart mime doc if it sees a FileUpload type. But it doesn't have
// code knowing about every concrete type to handle that.
func uploadAttachment(loc *cm15.RightScriptAttachmentLocator,
	file *rsapi.FileUpload, name string) error {
	var params rsapi.APIParams
	var p rsapi.APIParams
	APIVersion := "1.5"
	client := config.environment.Client15()

	p_inner := rsapi.APIParams{
		"content": file,
		"name":    name,
	}
	p = rsapi.APIParams{
		"right_script_attachment": p_inner,
	}
	uri, err := loc.ActionPath("RightScriptAttachment", "create")
	if err != nil {
		return err
	}
	req, err := client.BuildHTTPRequest(uri.HTTPMethod, uri.Path, APIVersion, params, p)
	if err != nil {
		return err
	}
	resp, err := client.PerformRequest(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := ioutil.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("invalid response %s: %s", resp.Status, string(respBody))
	}
	return nil
	//fmt.Printf("%#v", resp.Header)
	//location := resp.Header.Get("Location")
	// if len(location) == 0 {
	// 	return "", fmt.Errorf("Missing location header in response")
	// } else {
	// 	return location, nil
	// }
}

type RightScript struct {
	Href     string
	Path     string
	Metadata *RightScriptMetadata
}

func (r *RightScript) Push() error {
	client := config.environment.Client15()
	createLocator := client.RightScriptLocator("/api/right_scripts")
	apiParams := rsapi.APIParams{"filter": []string{"name==" + r.Metadata.Name}}
	rightscripts, err := createLocator.Index(apiParams)
	if err != nil {
		return err
	}
	foundId := ""
	for _, rs := range rightscripts {
		// Recheck the name here, filter does a impartial match and we need an exact one
		if rs.Name == r.Metadata.Name && rs.Revision == 0 {
			if foundId != "" {
				fatalError("Error, matched multiple RightScripts with the same name, please delete one: %d %d", rs.Id, foundId)
			} else {
				foundId = rs.Id
			}
		}
	}

	pathSrc, err := ioutil.ReadFile(r.Path)
	if err != nil {
		return err
	}

	var rightscriptLocator *cm15.RightScriptLocator
	if foundId == "" {
		fmt.Printf("Creating a new RightScript named '%s' from %s\n", r.Metadata.Name, r.Path)
		// New one, perform create call
		params := cm15.RightScriptParam2{
			Name:        r.Metadata.Name,
			Description: r.Metadata.Description,
			Source:      string(pathSrc),
		}
		rightscriptLocator, err = createLocator.Create(&params)
		fmt.Printf("  RightScript created with HREF %s\n", rightscriptLocator.Href)
	} else {
		href := fmt.Sprintf("/api/right_scripts/%s", foundId)
		fmt.Printf("Updating existing RightScript named '%s' with HREF %s from %s\n", r.Metadata.Name, href, r.Path)

		params := cm15.RightScriptParam3{
			Name:        r.Metadata.Name,
			Description: r.Metadata.Description,
			Source:      string(pathSrc),
		}
		rightscriptLocator = client.RightScriptLocator(href)
		err = rightscriptLocator.Update(&params)
		// Found existing, do an update
	}

	if err != nil {
		return err
	}

	attachmentsHref := fmt.Sprintf("%s/attachments", rightscriptLocator.Href)
	attachmentsLocator := client.RightScriptAttachmentLocator(attachmentsHref)
	attachments, err := attachmentsLocator.Index(rsapi.APIParams{})
	if err != nil {
		return err
	}

	toUpload := make(map[string]string)                           // scripts we want to upload
	onRightscript := make(map[string]*cm15.RightScriptAttachment) // scripts attached to the rightsript
	for _, a := range r.Metadata.Attachments {
		fullPath := filepath.Join(filepath.Dir(r.Path), a)
		md5, err := md5sum(fullPath)
		if err != nil {
			return err
		}
		toUpload[md5] = a
	}
	for _, a := range attachments {
		onRightscript[a.Digest] = a
	}

	// Two passes. First pass we delete RightScripts. This comes up when a file was
	// removed from the RightScript, or when the contents of a file on disk changed.
	// In the second case, the second pass will reupload the correct attachment.
	for digest, a := range onRightscript {
		if _, ok := toUpload[digest]; !ok {
			// HACK: self href for attachment is wrong for now. Back this out when its fixed
			scriptHref := ""
			for _, l := range a.Links {
				if l["rel"] == "right_script" {
					scriptHref = l["href"]
				}
			}
			href := fmt.Sprintf("%s/attachments/%d", scriptHref, a.Id)
			loc := client.RightScriptAttachmentLocator(href)
			fmt.Printf("  Deleting attachment '%s' with HREF '%s'\n", a.Name, href)
			err := loc.Destroy()
			if err != nil {
				return err
			}
		}
	}

	// Second pass, now upload any missing attachment and any attachments that were
	// deleted because we changed file contents.
	for digest, name := range toUpload {
		if _, ok := onRightscript[digest]; ok {
			fmt.Printf("  Attachment '%s' already uploaded with md5 %s\n", name, digest)
			// TBD -- update if a.Name != name?
		} else {
			fullPath := filepath.Join(filepath.Dir(r.Path), name)
			fmt.Printf("  Uploading attachment '%s' with md5 %s\n", name, digest)
			f, err := os.Open(fullPath)
			if err != nil {
				return err
			}
			// FileUpload represents payload fields that correspond to multipart file uploads.
			file := rsapi.FileUpload{Name: "right_script_attachment[content]", Reader: f, Filename: name}
			//params := cm15.RightScriptAttachmentParam{Content: &file, Name: a}
			err = uploadAttachment(attachmentsLocator, &file, path.Base(name))
			if err != nil {
				return err
			}
		}
	}

	return err
}

func fatalError(format string, v ...interface{}) {
	msg := fmt.Sprintf("ERROR: "+format, v...)
	fmt.Println(msg)
	os.Exit(1)
}

func validateRightScript(path string) error {
	script, err := os.Open(path)
	if err != nil {
		return err
	}
	defer script.Close()

	metadata, err := ParseRightScriptMetadata(script)
	if err != nil {
		return err
	}
	if *debug {
		pretty.Println(metadata)
	}
	fmt.Printf("%s - valid metadata\n", path)

	for _, attachment := range metadata.Attachments {
		fullPath := filepath.Join(filepath.Dir(path), attachment)

		md5, err := md5sum(fullPath)
		if err != nil {
			return err
		}
		fmt.Println(attachment, md5)
	}

	return nil
}

func md5sum(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := md5.New()

	_, err = io.Copy(hash, file)
	if err != nil {
		return "", err
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}
