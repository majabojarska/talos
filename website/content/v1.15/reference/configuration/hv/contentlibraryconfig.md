---
description: |
    ContentLibraryConfig is a content library configuration document.
    ContentLibraryConfig declares a content library: a store for virtual machine images
    kept on a volume.

    The library is a directory named after the document, created under
    `content-library/` on the backing volume, so a library `my-vm-images-1` backed by the
    user volume `u-vm-images` lives in `/var/mnt/vm-images/content-library/my-vm-images-1`.
    Several libraries may share a volume.

    Contents are managed over the API with `talosctl hv content-library`, never by editing
    the machine configuration. Status is reported via `ContentLibraryStatus`.
title: ContentLibraryConfig
---

<!-- markdownlint-disable -->









{{< highlight yaml >}}
apiVersion: v1alpha1
kind: ContentLibraryConfig
name: my-vm-images-1 # Name of the content library.
# Volume backing the content library.
backing:
    volume: u-vm-images # ID of the volume storing the library's contents.
{{< /highlight >}}


| Field | Type | Description | Value(s) |
|-------|------|-------------|----------|
|`name` |string |Name of the content library.<br><br>Must be between 1 and 63 characters long, and can only contain ASCII letters,<br>digits and hyphens. It names the library's directory on the backing volume, and<br>is the ID used to address the library over the API.  | |
|`backing` |<a href="#ContentLibraryConfig.backing">ContentLibraryBacking</a> |Volume backing the content library.  | |




## backing {#ContentLibraryConfig.backing}

ContentLibraryBacking describes the storage backing a content library.




| Field | Type | Description | Value(s) |
|-------|------|-------------|----------|
|`volume` |string |ID of the volume storing the library's contents.<br><br>The volume must be declared separately, and must be a non-system volume with a<br>filesystem: a user volume (`u-` prefix), an existing volume (`e-` prefix) or an<br>external volume (`x-` prefix). The volume is not provisioned by this document,<br>and the library becomes ready only once the volume is mounted. <details><summary>Show example(s)</summary>{{< highlight yaml >}}
volume: u-vm-images
{{< /highlight >}}</details> | |








