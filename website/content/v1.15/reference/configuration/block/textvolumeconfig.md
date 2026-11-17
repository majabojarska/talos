---
description: |
    TextVolumeConfig is a text volume configuration document.
    Text volumes hold a set of text files given inline in the machine configuration.

    The document does nothing on its own. Each consumer mounting it (e.g. a `ContainerConfig`
    `textVolume` mount) gets its own memory-backed copy of the files: no two consumers share an
    inode, even though the content is the same. The copy is always read-only, and its files
    cannot be executed directly. It is updated in place when the document changes, without
    restarting the consumer.

    Unlike user, existing and external volumes, a text volume has no shared mount on the host
    (there is nothing at `/var/mnt/<name>`), so it has no mount settings.

    The content is never written to persistent storage, but it is stored in the machine
    configuration verbatim, so treat it as being as sensitive as the machine configuration itself.

    The name must not be taken by a user, existing or external volume.
title: TextVolumeConfig
---

<!-- markdownlint-disable -->









{{< highlight yaml >}}
apiVersion: v1alpha1
kind: TextVolumeConfig
name: app-config # Name of the volume.
# Files in the volume, keyed by a path relative to the volume root.
files:
    app.conf: |
        [server]
        listen=:8080
    conf.d/log.ini: |
        level=info
{{< /highlight >}}


| Field | Type | Description | Value(s) |
|-------|------|-------------|----------|
|`name` |string |Name of the volume.<br><br>Name can only contain:<br>lowercase and uppercase ASCII letters, digits, and hyphens.  | |
|`files` |map[string]string |Files in the volume, keyed by a path relative to the volume root.<br><br>A path containing a slash creates subdirectories. Paths must be relative and may not<br>escape the volume with `..`, and names starting with `.talos-` are reserved. Content must<br>be valid UTF-8; files are created with mode `0644` and directories with `0755`. <details><summary>Show example(s)</summary>{{< highlight yaml >}}
files:
    app.conf: |
        [server]
        listen=:8080
    conf.d/log.ini: |
        level=info
{{< /highlight >}}</details> | |






