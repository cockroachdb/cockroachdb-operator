# This is the base image this application.
# If changes are made to this file, they will need to be pushed separately so the base image can be built in CI.
# After the image has been built, you can update the base image version in the WORKSPACE file.
# Based on UBI10 to keep Red Hat Partner Connect health grade at A/B (CC-35894).

FROM registry.access.redhat.com/ubi10/ubi-minimal:latest

RUN microdnf update -y && \
    microdnf install -y wget rpm procps-ng && \
    wget https://dl.fedoraproject.org/pub/epel/epel-release-latest-10.noarch.rpm && \
    rpm -ivh epel-release-latest-10.noarch.rpm && \
    microdnf install -y inotify-tools && \
    rm epel-release-latest-10.noarch.rpm && \
    microdnf clean all