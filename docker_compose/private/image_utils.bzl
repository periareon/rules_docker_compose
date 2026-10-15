"""Utilities for container images"""

ImageLoadRepositoryInfo = provider(
    doc = "Image content and declared tags for a given oci_load or image_load target",
    fields = {
        "oci_layout": "File (optional): The OCI layout directory for rules_oci images (contains index.json)",
        "tag_files": "List[File]: List of files containing tags (one per line, format: repository:tag) used for the load target",
        "tarball": "File (optional): An OCI layout tarball for rules_img images (the `tarball` output group of `image_load`)",
    },
)

def _image_load_repository_aspect_impl(target, ctx):
    # Handle rules_oci oci_load first (more specific - has repo_tags)
    # oci_load has 'image' attribute (OCI layout) and 'repo_tags' attribute (file with tags)
    if hasattr(ctx.rule.attr, "image") and ctx.rule.attr.image:
        # Check if this is rules_oci by looking for repo_tags attribute
        if hasattr(ctx.rule.file, "repo_tags") and ctx.rule.file.repo_tags:
            # This is rules_oci oci_load
            # Get the OCI layout from the image attribute
            oci_layout = None
            if hasattr(ctx.rule.file, "image") and ctx.rule.file.image:
                oci_layout = ctx.rule.file.image
            elif hasattr(ctx.rule.files, "image") and ctx.rule.files.image:
                oci_layout = ctx.rule.files.image[0]
            else:
                fail("oci_load target {} `image` attribute must provide files".format(target.label))

            # Use the repo_tags file directly as a tag_file
            repo_tags_file = ctx.rule.file.repo_tags

            return [ImageLoadRepositoryInfo(
                oci_layout = oci_layout,
                tarball = None,
                tag_files = [repo_tags_file],
            )]

    # Handle rules_img image_load
    # Check for image_load rule by looking for the 'image' attribute that provides ImageManifestInfo
    if hasattr(ctx.rule.attr, "image") and ctx.rule.attr.image:
        # Check if this is rules_img by looking for tag/tag_list/tag_file attributes
        if hasattr(ctx.rule.attr, "tag") or hasattr(ctx.rule.attr, "tag_list") or hasattr(ctx.rule.attr, "tag_file"):
            # This is likely rules_img image_load. Its `tarball` output group is
            # an OCI layout tarball with every blob embedded, which is what the
            # merger needs to derive digests and produce a loadable archive.
            if OutputGroupInfo not in target or not hasattr(target[OutputGroupInfo], "tarball"):
                fail("image_load target {} does not provide a `tarball` output group".format(target.label))
            tarballs = target[OutputGroupInfo].tarball.to_list()
            if len(tarballs) != 1:
                fail("image_load target {} `tarball` output group must contain exactly one file, got {}".format(
                    target.label,
                    len(tarballs),
                ))

            # Collect tags from tag, tag_list, or tag_file
            tags = []
            input_tag_file = None
            if hasattr(ctx.rule.attr, "tag") and ctx.rule.attr.tag:
                tags.append(ctx.rule.attr.tag)
            if hasattr(ctx.rule.attr, "tag_list") and ctx.rule.attr.tag_list:
                tags.extend(ctx.rule.attr.tag_list)
            if hasattr(ctx.rule.file, "tag_file") and ctx.rule.file.tag_file:
                input_tag_file = ctx.rule.file.tag_file

            if not tags and not input_tag_file:
                fail("image_load target {} must have at least one tag specified via `tag`, `tag_list`, or `tag_file`".format(target.label))

            # Build list of tag_files
            tag_files = []

            # If there's an existing tag_file, add it to the list
            if input_tag_file:
                tag_files.append(input_tag_file)

            # If there are string tags, write them to a new file and add it
            if tags:
                tag_file_output = ctx.actions.declare_file("{}.rules_docker_compose.tags.txt".format(target.label.name))
                ctx.actions.write(
                    output = tag_file_output,
                    content = "\n".join(tags),
                )
                tag_files.append(tag_file_output)

            return [ImageLoadRepositoryInfo(
                oci_layout = None,
                tarball = tarballs[0],
                tag_files = tag_files,
            )]

    # If we get here, this target doesn't match oci_load or image_load
    return []

# This aspect exists because rules_oci and rules_img don't provide a provider
# that cleanly publishes this information but for the docker_compose rules, it's
# absolutely necessary that an image's repository and digest are knowable.
# If rules_oci/rules_img decide to define their own provider for this (which they should)
# then this should be deleted in favor of that.
image_load_repository_aspect = aspect(
    doc = "Provides the image content and declared tags for a given oci_load or image_load target",
    implementation = _image_load_repository_aspect_impl,
)

# Keep the old name for backwards compatibility, but it's now an alias
image_push_repository_aspect = image_load_repository_aspect
