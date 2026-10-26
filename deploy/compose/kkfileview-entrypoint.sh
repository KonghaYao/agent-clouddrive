#!/bin/sh
set -eu
# kkFileView's native ConfigRefreshComponent and LocalOfficeUtils read this
# physical path independently of Spring's additional-location mechanism.
# Merge once so BOTH readers receive the same hardening configuration.
cat /app/upstream.properties /config/security.properties > /app/config/application.properties
exec java -Dfile.encoding=UTF-8 -Dspring.config.location=/app/config/application.properties -jar /app/kkfileview.jar
