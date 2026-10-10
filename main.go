package main

import (
  "context"
  "fmt"

  "go.podman.io/buildah"
  "go.podman.io/buildah/pkg/parse"
  "go.podman.io/common/pkg/config"
  "go.podman.io/image/v5/transports/alltransports"
  "go.podman.io/storage"
  "go.podman.io/storage/pkg/unshare"
)

func main() {
  if buildah.InitReexec() {
    return
  }
  unshare.MaybeReexecUsingUserNamespace(false)

  buildStoreOptions, err := storage.DefaultStoreOptions()
  if err != nil {
    panic(err)
  }

  conf, err := config.Default()
  if err != nil {
    panic(err)
  }
  capabilitiesForRoot, err := conf.Capabilities("root", nil, nil)
  if err != nil {
    panic(err)
  }

  buildStore, err := storage.GetStore(buildStoreOptions)
  if err != nil {
    panic(err)
  }
  defer buildStore.Shutdown(false)

  builderOpts := buildah.BuilderOptions{
    FromImage:        "docker.io/library/node:12-alpine",
    Capabilities:     capabilitiesForRoot,
  }

  builder, err := buildah.NewBuilder(context.TODO(), buildStore, builderOpts)
  if err != nil {
    panic(err)
  }
  defer builder.Delete()

  err = builder.Add("/home/node/", false, buildah.AddAndCopyOptions{}, "script.js")
  if err != nil {
    panic(err)
  }

  isolation, err := parse.IsolationOption("")
  if err != nil {
    panic(err)
  }

  err = builder.Run([]string{"sh", "-c", "date > /home/node/build-date.txt"}, buildah.RunOptions{Isolation: isolation, Terminal: buildah.WithoutTerminal})
  if err != nil {
    panic(err)
  }

  builder.SetCmd([]string{"node", "/home/node/script.js"})

  // Write the image as a tarball that `docker load` can import.
  imageRef, err := alltransports.ParseImageName("docker-archive:/output/my-image.tar:docker.io/myusername/my-image:latest")
  if err != nil {
    panic(err)
  }

  imageId, _, _, err := builder.Commit(context.TODO(), imageRef, buildah.CommitOptions{})
  if err != nil {
    panic(err)
  }

  fmt.Printf("Image built! %s\n", imageId)
}