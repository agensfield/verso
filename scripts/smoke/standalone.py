"""Exercise the built CLI in an isolated human PTY, with no real Codex or network."""

import base64, errno, json, os, pathlib, pty, select, subprocess, sys, tempfile, time
if len(sys.argv) != 2:
    raise SystemExit("usage: python3 scripts/smoke/standalone.py /path/to/verso")
binary=pathlib.Path(sys.argv[1]).resolve(strict=True)
with tempfile.TemporaryDirectory(prefix='verso-fixture-') as temporary:
    root=pathlib.Path(temporary); home=root/'home'; home.mkdir(mode=0o700)
    codex=home/'.codex'; codex.mkdir(mode=0o755); codex.chmod(0o755); state=root/'state'; tools=root/'bin'; tools.mkdir()
    def write(file,text,mode=0o600):
        file.write_text(text); file.chmod(mode)
    write(tools/'ps','#!/bin/sh\nexit 0\n',0o700)
    write(tools/'codex','#!/bin/sh\nif [ "$1" = "--version" ]; then echo "codex-cli 0.154.0"; else exit 99; fi\n',0o700)
    write(codex/'config.toml','cli_auth_credentials_store = "file"\n')
    def token(claims):
        return 'e30.'+base64.urlsafe_b64encode(json.dumps(claims).encode()).decode().rstrip('=')+'.synthetic'
    def auth(account):
        claims={'email':'same@example.test','https://api.openai.com/auth':{'chatgpt_user_id':'fixture-user','chatgpt_account_id':account}}
        return json.dumps({'tokens':{'id_token':token(claims),'access_token':token({'exp':4102444800}),'refresh_token':'synthetic-only','account_id':account}})
    # Explicitly isolated environment and loopback-only unusable HTTPS proxy.
    # No real Codex executable, home, daemon, credential, or external HTTP route.
    env={'PATH':str(tools)+':/usr/bin:/bin','HOME':str(home),'CODEX_HOME':str(codex),'VERSO_HOME':str(state),'HTTPS_PROXY':'http://127.0.0.1:1','HTTP_PROXY':'http://127.0.0.1:1','NO_PROXY':'','TERM':'xterm-256color'}
    for account in ['a','b']:
        write(codex/'auth.json',auth(account))
        result=subprocess.run([str(binary),'import',account],env=env,cwd=root,capture_output=True,text=True)
        assert result.returncode==0,result.stdout+result.stderr
    write(codex/'auth.json',auth('a'))
    master,slave=pty.openpty()
    process=subprocess.Popen([str(binary),'switch','b'],env=env,cwd=root,stdin=slave,stdout=slave,stderr=slave)
    os.close(slave); output=b''; approved=False; deadline=time.monotonic()+20
    while process.poll() is None and time.monotonic()<deadline:
        if select.select([master],[],[],0.1)[0]:
            try: output+=os.read(master,65536)
            except OSError as exc:
                if exc.errno != errno.EIO:
                    raise
                break
        if b'[y/N]' in output and not approved:
            os.write(master,b'y\n'); approved=True
    # PTY closure can precede the child becoming waitable by a few milliseconds.
    try:
        process.wait(timeout=max(0.001, deadline-time.monotonic()))
    except subprocess.TimeoutExpired:
        process.kill(); process.wait()
        raise AssertionError('fixture timed out: '+output.decode(errors='replace'))
    os.close(master)
    assert approved and process.returncode==0,output.decode(errors='replace')
    selected=json.loads((codex/'auth.json').read_text())['tokens']['account_id']
    assert selected=='b',selected
    assert not (state/'switch.json').exists()
    assert not (state/'herdr-snapshot.json').exists()
    assert b'managed' in output.lower(),output.decode(errors='replace')
    print('PASS: real binary + human PTY, standalone a -> b, no native daemon invocation, no live home/auth/network, journal cleared, no Herdr snapshot.')
