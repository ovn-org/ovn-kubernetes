We are utilizing [GitHub Pages](https://docs.github.com/en/pages/quickstart) to host the ovn-kubernetes.io website. The website's
content is composed in Markdown and stored within the 'docs' directory at the root
of our repository. For static site generation, we employ the [MkDocs](https://www.mkdocs.org/user-guide/writing-your-docs/) framework,
managed by the 'mkdocs.yml' configuration file located next to the 'docs' folder.
Additionally, we use the [Material](https://squidfunk.github.io/mkdocs-material/setup/) framework on top of  MkDocs, which enhances our
site with advanced features like customizable navigation, color schemes, and site
search capabilities

Any changes to the files in the doc folder or the mkdocs.yml file is picked up by
a GitHub Action workflow that will automatically publish the changes to the website.

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
