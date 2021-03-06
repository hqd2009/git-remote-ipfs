/*
git-remote-helper implements a git-remote helper that uses the ipfs transport.

TODO

Currently assumes a IPFS Daemon at localhost:5001

Not completed: new Push (issue #2), IPNS, URLs like fs:/ipfs/.. (issue #3), embedded IPFS node

...

 $ git clone ipfs://ipfs/$hash/repo.git
 $ cd repo && make $stuff
 $ git commit -a -m 'done!'
 $ git push origin
 => clone-able as ipfs://ipfs/$newHash/repo.git

Links

https://ipfs.io

https://github.com/whyrusleeping/git-ipfs-rehost

https://git-scm.com/docs/gitremote-helpers

https://git-scm.com/book/en/v2/Git-Internals-Plumbing-and-Porcelain

https://git-scm.com/docs/gitrepository-layout

https://git-scm.com/book/en/v2/Git-Internals-Transfer-Protocols
*/
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/cryptix/git-remote-ipfs/internal/path"

	"github.com/cryptix/go/logging"
	"github.com/ipfs/go-ipfs-shell"
	"gopkg.in/errgo.v1"
)

const usageMsg = `usage git-remote-ipfs <repository> [<URL>]
supports:

* ipfs://ipfs/$hash/path..
* ipfs:///ipfs/$hash/path..

`

func usage() {
	fmt.Fprint(os.Stderr, usageMsg)
	os.Exit(2)
}

var (
	ref2hash = make(map[string]string)

	ipfsShell     = shell.NewShell("localhost:5001")
	ipfsRepoPath  string
	thisGitRepo   string
	thisGitRemote string
	errc          chan<- error
	log           = logging.Logger("git-remote-ipfs")
)

func main() {
	// logging
	logging.SetupLogging(nil)

	// env var and arguments
	thisGitRepo = os.Getenv("GIT_DIR")
	if thisGitRepo == "" {
		log.Fatal("could not get GIT_DIR env var")
	}
	if thisGitRepo == ".git" {
		cwd, err := os.Getwd()
		logging.CheckFatal(err)
		thisGitRepo = filepath.Join(cwd, ".git")
	}
	log.Debug("GIT_DIR=", thisGitRepo)

	var u string // repo url
	v := len(os.Args[1:])
	switch v {
	case 2:
		thisGitRemote = os.Args[1]
		u = os.Args[2]
		log.Debug("remote:", thisGitRemote)
		log.Debug("url:", u)
	default:
		log.Fatalf("usage: unknown # of args: %d\n%v", v, os.Args[1:])
	}

	// parse passed URL
	for _, pref := range []string{"ipfs://ipfs/", "ipfs:///ipfs/"} {
		if strings.HasPrefix(u, pref) {
			u = "/ipfs/" + u[len(pref):]
			log.Debug("prefix cut:", u)
		}
	}
	p, err := path.ParsePath(u)
	if err != nil {
		log.Fatalf("path.ParsePath() failed: %s", err)
	}
	ipfsRepoPath = p.String()

	// interrupt / error handling
	go func() {
		if err := interrupt(); err != nil {
			log.Fatal("interrupted:", err)
		}
	}()

	if err := speakGit(os.Stdin, os.Stdout); err != nil {
		log.Fatal("speakGit failed:", err)
	}
}

// speakGit acts like a git-remote-helper
// see this for more: https://www.kernel.org/pub/software/scm/git/docs/gitremote-helpers.html
func speakGit(r io.Reader, w io.Writer) error {
	//debugLog := logging.Logger("git")
	//r = debug.NewReadLogrus(debugLog, r)
	//w = debug.NewWriteLogrus(debugLog, w)
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		text := scanner.Text()
		switch {

		case text == "capabilities":
			fmt.Fprintln(w, "fetch")
			fmt.Fprintln(w, "push")
			fmt.Fprintln(w, "")

		case strings.HasPrefix(text, "list"):
			log.Debug("got list line")
			var (
				forPush = strings.Contains(text, "for-push")
				err     error
				head    string
			)
			if err = listInfoRefs(forPush); err == nil { // try .git/info/refs first
				if head, err = listHeadRef(); err != nil {
					return err
				}
			} else { // alternativly iterate over the refs directory like git-remote-dropbox
				if forPush {
					log.Info("for-push: should be able to push to non existant.. TODO #2")
				}
				log.WithField("err", err).Debug("didn't find info/refs in repo, falling back...")
				if err = listIterateRefs(forPush); err != nil {
					return err
				}
			}
			if len(ref2hash) == 0 {
				return errgo.New("did not find _any_ refs...")
			}
			// output
			for ref, hash := range ref2hash {
				if head == "" && strings.HasSuffix(ref, "master") {
					// guessing head if it isnt set
					head = hash
				}
				fmt.Fprintf(w, "%s %s\n", hash, ref)
			}
			fmt.Fprintf(w, "%s HEAD\n", head)
			fmt.Fprintln(w, "")

		case strings.HasPrefix(text, "fetch "):
			for scanner.Scan() {
				fetchSplit := strings.Split(text, " ")
				if len(fetchSplit) < 2 {
					return errgo.Newf("malformed 'fetch' command. %q", text)
				}
				f := map[string]interface{}{
					"sha1": fetchSplit[1],
					"name": fetchSplit[2],
				}
				err := fetchObject(fetchSplit[1])
				if err == nil {
					log.WithFields(f).Debug("fetched loose")
					fmt.Fprintln(w, "")
					continue
				}
				log.WithFields(f).WithField("err", err).Debug("fetchLooseObject failed, trying packed...")
				err = fetchPackedObject(fetchSplit[1])
				if err != nil {
					return errgo.Notef(err, "fetchPackedObject() failed")
				}
				log.WithFields(f).Debug("fetched packed")
				text = scanner.Text()
				if text == "" {
					break
				}
			}
			fmt.Fprintln(w, "")

		case strings.HasPrefix(text, "push"):
			for scanner.Scan() {
				pushSplit := strings.Split(text, " ")
				if len(pushSplit) < 2 {
					return errgo.Newf("malformed 'push' command. %q", text)
				}
				srcDstSplit := strings.Split(pushSplit[1], ":")
				if len(srcDstSplit) < 2 {
					return errgo.Newf("malformed 'push' command. %q", text)
				}
				src, dst := srcDstSplit[0], srcDstSplit[1]
				f := map[string]interface{}{
					"src": src,
					"dst": dst,
				}
				log.WithFields(f).Debug("got push")
				if src == "" {
					fmt.Fprintf(w, "error %s %s\n", dst, "delete remote dst: not supported yet - please open an issue on github")
				} else {
					if err := push(src, dst); err != nil {
						fmt.Fprintf(w, "error %s %s\n", dst, err)
						return err
					}
					fmt.Fprintln(w, "ok", dst)
				}
				text = scanner.Text()
				if text == "" {
					break
				}
			}
			fmt.Fprintln(w, "")

		case text == "":
			break

		default:
			return errgo.Newf("Error: default git speak: %q", text)
		}
	}
	if err := scanner.Err(); err != nil {
		return errgo.Notef(err, "scanner.Err()")
	}
	return nil
}
