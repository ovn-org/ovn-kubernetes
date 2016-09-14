How to Submit Patches for ovn-kubernetes
========================================

Create github pull requests for the repo.  More details are included below.

Before You Start
----------------

Before you send patches at all, make sure that each patch makes sense.
In particular:

  - A given patch should not break anything, even if later
    patches fix the problems that it causes.  The source tree
    should still work after each patch is applied.  (This enables
    `git bisect` to work best.)

  - A patch should make one logical change.  Don't make
    multiple, logically unconnected changes to disparate
    subsystems in a single patch.

  - A patch that adds or removes user-visible features should
    also update the appropriate user documentation or manpages.

Commit Summary
-------------

The summary line of your commit should be in the following format:
`<area>: <summary>`

  - `<area>:` indicates the area of the ovn-kubernetes to which the
    change applies (often the name of a source file or a
    directory).  You may omit it if the change crosses multiple
    distinct pieces of code.

  - `<summary>` briefly describes the change.

Commit Description
------------------

The body of the commit message should start with a more thorough description of
the change.  This becomes the body of the commit message, following
the subject.  There is no need to duplicate the summary given in the
subject.

Please limit lines in the description to 79 characters in width.

The description should include:

  - The rationale for the change.

  - Design description and rationale (but this might be better
    added as code comments).

  - Testing that you performed (or testing that should be done
    but you could not for whatever reason).

  - Tags (see below).

There is no need to describe what the patch actually changed, if the
reader can see it for himself.

If the patch refers to a commit already in the ovn-kubernetes
repository, please include both the commit number and the subject of
the patch, e.g. 'commit 632d136c (ovn-k8s-overlay: Flush the IP address
of physical interface)

Tags
----

The description ends with a series of tags, written one to a line as
the last paragraph of the email.  Each tag indicates some property of
the patch in an easily machine-parseable manner.

Examples of common tags follow.

    Signed-off-by: Author Name <author.name@email.address...>

        Informally, this indicates that Author Name is the author or
        submitter of a patch and has the authority to submit it under
        the terms of the license.  The formal meaning is to agree to
        the Developer's Certificate of Origin (see below).

        If the author and submitter are different, each must sign off.
        If the patch has more than one author, all must sign off.

        Signed-off-by: Author Name <author.name@email.address...>
        Signed-off-by: Submitter Name <submitter.name@email.address...>

    Co-authored-by: Author Name <author.name@email.address...>

        Git can only record a single person as the author of a given
        patch.  In the rare event that a patch has multiple authors,
        one must be given the credit in Git and the others must be
        credited via Co-authored-by: tags.  (All co-authors must also
        sign off.)

    Acked-by: Reviewer Name <reviewer.name@email.address...>

        Reviewers will often give an Acked-by: tag to code of which
        they approve.  It is polite for the submitter to add the tag
        before posting the next version of the patch or applying the
        patch to the repository.  Quality reviewing is hard work, so
        this gives a small amount of credit to the reviewer.

        Not all reviewers give Acked-by: tags when they provide
        positive reviews.  It's customary only to add tags from
        reviewers who actually provide them explicitly.

    Tested-by: Tester Name <reviewer.name@email.address...>

        When someone tests a patch, it is customary to add a
        Tested-by: tag indicating that.  It's rare for a tester to
        actually provide the tag; usually the patch submitter makes
        the tag himself in response to an email indicating successful
        testing results.

    Tested-at: <URL>

        When a test report is publicly available, this provides a way
        to reference it.  Typical <URL>s would be build logs from
        autobuilders or references to mailing list archives.

        Some autobuilders only retain their logs for a limited amount
        of time.  It is less useful to cite these because they may be
        dead links for a developer reading the commit message months
        or years later.

    Reported-by: Reporter Name <reporter.name@email.address...>

        When a patch fixes a bug reported by some person, please
        credit the reporter in the commit log in this fashion.  Please
        also add the reporter's name and email address to the list of
        people who provided helpful bug reports in the AUTHORS file at
        the top of the source tree.

        Fairly often, the reporter of a bug also tests the fix.
        Occasionally one sees a combined "Reported-and-tested-by:" tag
        used to indicate this.  It is also acceptable, and more
        common, to include both tags separately.

        (If a bug report is received privately, it might not always be
        appropriate to publicly credit the reporter.  If in doubt,
        please ask the reporter.)

    Requested-by: Requester Name <requester.name@email.address...>
    Suggested-by: Suggester Name <suggester.name@email.address...>

        When a patch implements a request or a suggestion made by some
        person, please credit that person in the commit log in this
        fashion.  For a helpful suggestion, please also add the
        person's name and email address to the list of people who
        provided suggestions in the AUTHORS file at the top of the
        source tree.

        (If a suggestion or a request is received privately, it might
        not always be appropriate to publicly give credit.  If in
        doubt, please ask.)

    Reported-at: <URL>

        If a patch fixes or is otherwise related to a bug reported in
        a public bug tracker, please include a reference to the bug in
        the form of a URL to the specific bug, e.g.:

        Reported-at: https://bugs.debian.org/743635

        This is also an appropriate way to refer to bug report emails
        in public email archives, e.g.:

        Reported-at: http://openvswitch.org/pipermail/dev/2014-June/040952.html

    VMware-BZ: #1234567
    ONF-JIRA: EXT-12345

        If a patch fixes or is otherwise related to a bug reported in
        a private bug tracker, you may include some tracking ID for
        the bug for your own reference.  Please include some
        identifier to make the origin clear, e.g. "VMware-BZ" refers
        to VMware's internal Bugzilla instance and "ONF-JIRA" refers
        to the Open Networking Foundation's JIRA bug tracker.

    Fixes: 63bc9fb1c69f (“packets: Reorder CS_* flags to remove gap.”)

        If you would like to record which commit introduced a bug being fixed,
        you may do that with a “Fixes” header.  This assists in determining
        which OVS releases have the bug, so the patch can be applied to all
        affected versions.  The easiest way to generate the header in the
        proper format is with this git command:

        git log -1 --pretty=format:"Fixes: %h (\"%s\")" --abbrev=12 COMMIT_REF

    Vulnerability: CVE-2016-2074

        Specifies that the patch fixes or is otherwise related to a
        security vulnerability with the given CVE identifier.  Other
        identifiers in public vulnerability databases are also
        suitable.

        If the vulnerability was reported publicly, then it is also
        appropriate to cite the URL to the report in a Reported-at
        tag.  Use a Reported-by tag to acknowledge the reporters.

Developer's Certificate of Origin
---------------------------------

To help track the author of a patch as well as the submission chain,
and be clear that the developer has authority to submit a patch for
inclusion in openvswitch please sign off your work.  The sign off
certifies the following:

    Developer's Certificate of Origin 1.1

    By making a contribution to this project, I certify that:

    (a) The contribution was created in whole or in part by me and I
        have the right to submit it under the open source license
        indicated in the file; or

    (b) The contribution is based upon previous work that, to the best
        of my knowledge, is covered under an appropriate open source
        license and I have the right under that license to submit that
        work with modifications, whether created in whole or in part
        by me, under the same open source license (unless I am
        permitted to submit under a different license), as indicated
        in the file; or

    (c) The contribution was provided directly to me by some other
        person who certified (a), (b) or (c) and I have not modified
        it.

    (d) I understand and agree that this project and the contribution
        are public and that a record of the contribution (including all
        personal information I submit with it, including my sign-off) is
        maintained indefinitely and may be redistributed consistent with
        this project or the open source license(s) involved.
