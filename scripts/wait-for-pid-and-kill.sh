#!/usr/bin/env bash
set -euo pipefail
set +x

# wait-for-pid-and-kill.sh <filename> <max wait seconds>
##
# Once per second it will check for the existence of the passed file and if its
# there, will read a process id from it and issue a kill to that process.
# If the file hasn't appeared within <max wait seconds> it exits.
#
# We use this script in the make generate process to start the bazel server in
# a subprocess and then kill that process which prevents make from shutting
# down.

pid_file=$1;
max_wait_seconds=$2;

end_seconds=$((SECONDS+$max_wait_seconds));
while [ $SECONDS -lt $end_seconds ]; do
	if [ -f "$pid_file" ]; then
		# only kill the process if it was running
		if [ "$(pgrep -F "$pid_file" | wc -l)" -ge 1 ]; then
			pkill -F "$pid_file"
		fi
		rm -f "$pid_file";
		exit 0;
	fi
	sleep 1;
done

echo "Timed out waiting for $pid_file to appear" 1>&2

exit 1;
