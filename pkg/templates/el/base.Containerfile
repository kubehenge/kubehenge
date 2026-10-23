# The minimal root file system build.sh composed, as a bootable container image.
FROM scratch
COPY rootfs/ /
LABEL containers.bootc=1
LABEL ostree.bootable=1
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
