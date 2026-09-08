---
description: |
    TextFilesConfig is a set of text files to be presented to a container.
    TextFilesConfig declares a named set of text files, given inline as content keyed by a
    relative path.

    The document holds content only: it does nothing on its own until a `ContainerConfig`
    mounts it via a `textFiles` mount, at which point the paths are materialized as a real
    directory tree and bind-mounted read-only at the mount's destination. Nested paths become
    real subdirectories.

    The tree is rebuilt from the machine configuration on every boot and is never written to
    persistent storage, but the contents are stored in the machine configuration verbatim, so
    treat anything put here as being as sensitive as the machine configuration itself.
title: TextFilesConfig
---

<!-- markdownlint-disable -->









{{< highlight yaml >}}
apiVersion: v1alpha1
kind: TextFilesConfig
name: my-foobar-configs # Name of the file set.
# Files in the set, keyed by a path relative to the mount destination.
files:
    foo.conf: |
        [section-abc]
        key1=val1
        key2=val2
    subdir/bar.ini: |
        key1=val1
{{< /highlight >}}


| Field | Type | Description | Value(s) |
|-------|------|-------------|----------|
|`name` |string |Name of the file set.<br><br>Must be between 1 and 63 characters long, and can only contain lowercase ASCII<br>letters, digits and hyphens. It is the name a `ContainerConfig` refers to, and is used<br>as a directory name on the host.  | |
|`files` |map[string]string |Files in the set, keyed by a path relative to the mount destination.<br><br>A key containing a slash creates subdirectories. Keys must be relative and may not<br>escape the set with `..`. Content must be valid UTF-8; files are created with mode<br>`0644` and directories with `0755`. <details><summary>Show example(s)</summary>{{< highlight yaml >}}
files:
    foo.conf: |
        [section-abc]
        key1=val1
        key2=val2
    subdir/bar.ini: |
        key1=val1
{{< /highlight >}}</details> | |






