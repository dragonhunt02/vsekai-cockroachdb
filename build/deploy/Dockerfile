FROM fedora:41
RUN dnf update -y && dnf install -y tar gzip xz tzdata hostname && dnf clean all
# ubi10 is required because oxide-computer builds require glibc 2.35 minimum
# Replace with line below when ubi10 minimal exits beta and uncomment microdnf commands
# FROM registry.access.redhat.com/ubi10/ubi-minimal

# For deployment, we need the following additionally installed:
# tzdata - for time zone functions; reinstalled to replace the missing
#          files in /usr/share/zoneinfo/
# hostname - used in cockroach k8s manifests
# tar - used by kubectl cp
#RUN microdnf update -y \
#    && rpm --erase --nodeps tzdata \
#    && microdnf install tzdata hostname tar gzip xz -y \
#    && rm -rf /var/cache/yum

RUN mkdir /usr/local/lib/cockroach /cockroach /licenses /docker-entrypoint-initdb.d
COPY cockroach.sh cockroach /cockroach/
COPY licenses/* /licenses/
# Install GEOS libraries.
COPY libgeos.so libgeos_c.so /usr/local/lib/cockroach/

# Set working directory so that relative paths
# are resolved appropriately when passed as args.
WORKDIR /cockroach/

# Include the directory in the path to make it easier to invoke
# commands via Docker
ENV PATH=/cockroach:$PATH

ENV COCKROACH_CHANNEL=official-docker

EXPOSE 26257 8080
ENTRYPOINT ["/cockroach/cockroach.sh"]
