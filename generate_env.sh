#!/bin/bash
# generate_env.sh — MOCK placeholder for testing
#
# The PCDS server calls this script once per new student:
#   bash generate_env.sh "<student_name>" "<uuid_domain>"
#
# $1 = student name  (e.g. "Alice Smith")
# $2 = student UUID domain  (e.g. "bchd.c2af2850-f31d-42f3-9b40-37b32f1824b5")
#
# Print only export statements and comments to stdout — this output is stored
# in students.json and delivered to the student's machine every time they run
# the setup script. Send any progress/error messages to stderr instead.
#
# Replace this file with your real provisioning logic. For an Azure Terraform
# example that creates a resource group and service principal per student, see
# the Azure example in the project docs.

STUDENT_NAME="$1"
STUDENT_UUID="$2"

MOCK_TOKEN=$(echo -n "${STUDENT_UUID}" | sha256sum | awk '{print $1}' | cut -c1-24)

echo "# PCDS config for ${STUDENT_NAME} (${STUDENT_UUID})"
echo "export PCDS_STUDENT_NAME=\"${STUDENT_NAME}\""
echo "export PCDS_UUID_DOMAIN=\"${STUDENT_UUID}\""
echo "export PCDS_MOCK_TOKEN=\"${MOCK_TOKEN}\""
