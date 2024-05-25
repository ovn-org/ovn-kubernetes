Documentation is an important step in the SDLC process. Unless there are
proper docs to tell end users about the code you contributed; there will
be less visibility and understanding of your code and feature. It will also
become hard to maintain such code.

## Writing Documentation

All OVN-Kubernetes docs are kept under the `docs/` folder in the main path.
There are specific folders such as `features` and `developer-guide` where
you can add your commit against. These simple `.md` files are referred from
the navigation tile in `mkdocs.yaml` file which is what is used to
build our [website](https://ovn-kubernetes.io/).

As a developer and contributor to this project; it is recommended that you
include a docs commit in your PR whenever possible and relevant. Some examples
where we mandate docs include:

* **Enhancement Proposals**: If you are planning to do a new feature, then start
with an enhancement proposal a.k.a OKEP so that maintainers and other reviewers
can get an understanding of what your goals are. See [here](https://ovn-kubernetes.io/okeps/okep-4368-template/)
for more details and open a commit adding it to our `docs/okeps` folder.

* **Architecture and Topology docs**: Did you change the architecture or topology?
If so please write docs reflecting your design changes and update any existing docs
so that they remain relevant. Open a commit adding it to our `docs/design` folder.

* **Feature Docs**: If your enhancement proposal has merged; next step is to
implement that feature. As part of the main implementation PR we mandate adding a
feature documentation commit. See [here](https://github.com/ovn-org/ovn-kubernetes/blob/master/docs/features/template.md)
for how a feature documentation should be done. Open a commit adding it to our
`docs/features` folder.

* **Contributor Docs**: If your changes include code cleanup or refactoring
that is not user facing but internal; example: "Adding DBIndexes to AddressSets" write up a
developer guide for contributors asking them to follow that new code structure process while
adding new AddressSets. Open a commit adding it to our `docs/developer-guide` folder.

* **Bug Fixes**: If there was a difficult or critical bug fix that warrants a doc; please
feel free to write it. If you are unsure where this should be placed; reach out to the maintainers.

* **Blog Posts**: Are you an end-user of OVN-Kubernetes? Is there something you wish to share with
the community about your awesome use cases and how you used our CNI to solve your problems? We
welcome blog post contributions from all! See [here](https://ovn-kubernetes.io/blog/) for details.
Open a commit adding it to our `docs/blog` folder.

* **Performance Enhancements**: We love performance enhancements! Did you write a cool patch
to reduce the time it takes for iptables to sync up on startup? Think about writing a good blog post
around this! Open a commit adding it to our `docs/blog` folder.

* **API Reference**: Did you introduce a new CRD? OR Did you add a watcher a new CRD? Include API
Reference documentation changes to `docs/api-reference` folder. See [here](https://ovn-kubernetes.io/api-reference/introduction/)
for more details.

## Website Guide

We are utilizing [GitHub Pages](https://docs.github.com/en/pages/quickstart) to host the ovn-kubernetes.io website. The website's
content is composed in Markdown and stored within the 'docs' directory at the root
of our repository. For static site generation, we employ the [MkDocs](https://www.mkdocs.org/user-guide/writing-your-docs/) framework,
managed by the 'mkdocs.yml' configuration file located next to the 'docs' folder.
Additionally, we use the [Material](https://squidfunk.github.io/mkdocs-material/setup/) framework on top of  MkDocs, which enhances our
site with advanced features like customizable navigation, color schemes, and site
search capabilities

Any changes to the files in the doc folder or the mkdocs.yml file is picked up by
a GitHub Action workflow that will automatically publish the changes to the website.

## How to test your documentation changes?

In order to test changes locally to either mkdocs.yml or to files under docs/ folder,
please follow the instructions below.

## Clone the repository
```text
# git clone https://github.com/ovn-org/ovn-kubernetes
Cloning into 'ovn-kubernetes'...
remote: Enumerating objects: 84258, done.
remote: Counting objects: 100% (1546/1546), done.
remote: Compressing objects: 100% (686/686), done.
remote: Total 84258 (delta 809), reused 1208 (delta 663), pack-reused 82712
Receiving objects: 100% (84258/84258), 56.39 MiB | 28.74 MiB/s, done.
Resolving deltas: 100% (55993/55993), done.

# cd ovn-kubernetes
```

## Create a Python virtual environment
```python
#  python -m venv venv
```

## Activate the virtual environment
```text
# source venv/bin/activate
```

## Install all the required python packages to render the website
```text
(venv) # pip install -r requirements.txt 
Collecting Click
  Using cached click-8.1.7-py3-none-any.whl (97 kB)
Collecting htmlmin

<output snipped for brevity>
```
## Run the website locally using
```text
(venv) # mkdocs serve
INFO    -  Building documentation...
INFO    -  Cleaning site directory

<output snipped for brevity>

INFO    -  Documentation built in 1.52 seconds
INFO    -  [17:05:21] Watching paths for changes: 'docs', 'mkdocs.yml'
INFO    -  [17:05:21] Serving on http://127.0.0.1:8000/ovn-kubernetes/
```
Now you can browse the website on your browser using the above URL.

As you make changes and save the files, the mkdocs server notices that and re-builds the website.
It also spews out any WARNINGS or ERRORS with respect to the changes that you have just made. If
the changes look good, then go ahead and submit the PR.
