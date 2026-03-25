import paramiko
import sys

ssh = paramiko.SSHClient()
ssh.set_missing_host_key_policy(paramiko.AutoAddPolicy())

try:
    ssh.connect('152.32.229.228', port=6622, username='root', password='HHHnmb123', timeout=10)
    stdin, stdout, stderr = ssh.exec_command('uname -a; hostname; uptime; df -h /')
    print(stdout.read().decode())
    err = stderr.read().decode()
    if err:
        print("ERR:", err, file=sys.stderr)
    ssh.close()
    print("\n=== Connection successful ===")
except Exception as e:
    print(f"Error: {e}", file=sys.stderr)
    sys.exit(1)
