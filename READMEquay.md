# Building with quay.io

## quay.io repositories

quay.io is a convenient place to build and store your development
docker images.  Set up an account for yourself and then set up
an automatic build of your ovn-kubernetes fork. The build is
triggered  whenever you push changes to your fork. The build is
on the master branch.  The result is
```
quay.io/repository/<your-account>/ovn-kube:latest
```
You can use the image in your daemonsets for development.

Dockerfile.centos builds the binaries and then includes them in the
image.


## Create an account on quay.io

Create an account on quay.io and link it to your github account.
Browse to https://quay.io/signin

Just press the "login with github" button and ignore the other fields.

After login, Press the "+ Create New Repository" (top right of screen)

On the "Create New Repository" screen provide a repository name (e.g., ovn-kube)
and select "public". This will bring up additional selections. Select
"Link to a GitHub Repository Push"

This brings up the "Setup Build Trigger" screen.
Select organization that is your user name. This brings up a list of repos in your
account. Select "ovn-kubernetes". Press "continue".

Screen extends with "Select Trigger". Take the default, press "continue".

Screen extends with "Select Dockerfile"
Enter "/Dockerfile.centos", press "continue".

Screen extends with "Select Context"
Enter "/", press "continue".

Screen extends with "Optional Robot Account"
Press "continue".

'Click "Continue" to complete setup of this build trigger.'
Press "continue".

On next screen, press the button at the bottom right. In my case,
"Return to pecameron/ovn-phil"

## Quay Builds
When you set up quay.io as above, it will build the master branch in
your github your-github-name/ovn-kubernetes repo.

Whenever you push to your github ovn-kubernetes repo a new build is
automatically triggered. It is labeled **latest** and **master**.

You can force a new build as follows.  On "your-account / ovn-kube",
press the "Start New Build" button and select desired options.

## Reference the Image
In the three daemonset .yaml files set the image to:

```
spec:
  template:
    spec:
      containers:
      - name: ovnkube
        image: quay.io/repository/<your-account>/ovn-kube:latest
```
