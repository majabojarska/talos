---
description: |
    TextVolumeConfig is a text volume configuration document.
    Text volume is a set of text files kept in memory (`tmpfs`) and mounted under `/var/mnt/<name>`.
    It can be mounted into Talos containers with a `volume` mount in `ContainerConfig`, similar to a Kubernetes ConfigMap.
    The mount must set the `ro` option, as text volumes are read-only for containers.
    The name must not be taken by a user, existing or external volume.

    Changes to the files are applied in place, without restarting the containers which mount the volume:
    the files are replaced atomically as a whole, by swapping the `..data` symbolic link
    to a directory holding the new set of files.
    Each top-level entry of the volume is a symbolic link into `..data`.
title: TextVolumeConfig
---

<!-- markdownlint-disable -->









{{< highlight yaml >}}
apiVersion: v1alpha1
kind: TextVolumeConfig
name: app-config # Name of the volume.
# Files of the volume, keyed by the path relative to the volume root.
files:
    app.yaml:
        contents: | # The contents of the file.
            listen: :8080
    conf.d/extra.ini:
        mode: 0o600 # The file's permissions in octal.
        contents: | # The contents of the file.
            [section]
            key=value
{{< /highlight >}}


| Field | Type | Description | Value(s) |
|-------|------|-------------|----------|
|`name` |string |Name of the volume.<br><br>Name might be between 1 and 34 characters long and can only contain:<br>lowercase and uppercase ASCII letters, digits, and hyphens.  | |
|`secret` |bool |Marks the file contents as secret.<br><br>Contents of a secret volume are replaced when the secrets of the machine configuration are redacted,<br>e.g. when the configuration is shown to a user without access to secrets.<br>The files are still rendered in full into the volume.<br><br>Defaults to `false`.  | |
|`files` |<a href="#TextVolumeConfig.files.-">map[string]TextVolumeFileV1Alpha1</a> |Files of the volume, keyed by the path relative to the volume root.<br><br>Paths may contain subdirectories, which are created automatically.<br>Path components must not start with `..`.<br>The total size of the file contents must not exceed 1 MiB.  | |




## files.* {#TextVolumeConfig.files.-}

TextVolumeFileV1Alpha1 describes a single file of a text volume.




| Field | Type | Description | Value(s) |
|-------|------|-------------|----------|
|`mode` |TextVolumeFileMode |The file's permissions in octal.<br><br>Defaults to `0o644`.  | |
|`contents` |string |The contents of the file.  | |








