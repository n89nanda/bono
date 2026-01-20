import json
import os
import subprocess
import sys
import termios
import time
import tty

from dotenv import load_dotenv
from openai import OpenAI

load_dotenv()
client = OpenAI(
    base_url=os.environ.get("BASE_URL", "https://openrouter.ai/api/v1"),
    api_key=os.environ["OPENROUTER_API_KEY"],
)
model = os.environ.get("MODEL", "anthropic/claude-opus-4.5")

with open("tools.json", "r") as f:
    tools = json.load(f)


def getch():
    fd = sys.stdin.fileno()
    old = termios.tcgetattr(fd)
    try:
        tty.setraw(fd)
        return sys.stdin.read(1)
    finally:
        termios.tcsetattr(fd, termios.TCSADRAIN, old)


def run_tool(name, args):
    marker = "● "
    if name == "write_file":
        prompt = (
            f"{marker}Write('{args['path']}', {len(args['content'].splitlines())} lines)"
        )
        run = lambda: (open(args["path"], "w").write(args["content"]), "written", "ok")[
            1:
        ]
    elif name == "read_file":
        prompt = f"{marker}Read('{args['path']}')"
        run = lambda: (f"{len((c := open(args['path']).read()).splitlines())} lines", c)
    elif name == "edit_file":
        prompt = f"{marker}Edit('{args['path']}')"
        def run():
            content = open(args["path"]).read()
            old, new = args["old_string"], args["new_string"]
            count = content.count(old)
            if count == 0:
                return "fail: string not found", ""
            if count > 1 and not args.get("replace_all"):
                return f"fail: {count} matches (use replace_all)", ""
            open(args["path"], "w").write(content.replace(old, new, -1 if args.get("replace_all") else 1))
            return "ok", f"replaced {count} occurrence(s)"
    else:
        desc = args.get("description", "(no description)")
        safety = args.get("safety", "modify")
        prompt = f"{marker}Bash('{args['command']}') # {desc}, {safety}"

        def run():
            t = time.time()
            r = subprocess.run(
                args["command"], shell=True, capture_output=True, text=True
            )
            return (
                f"{'ok' if r.returncode == 0 else 'fail'} ({time.time() - t:.1f}s)",
                r.stdout + r.stderr,
            )

    if name == "read_file":
        print(prompt, end=" ", flush=True)
    else:
        print(prompt, end=" [Enter/Esc] ", flush=True)
        if getch() == "\x1b":
            print("=> cancelled")
            return None, False
    status, result = run()
    print(f"\r{prompt} => {status}" + " " * 20)
    return result, True


with open("system_prompt.txt", "r") as f:
    system_prompt = f.read()

messages = [
    {
        "role": "system",
        "content": system_prompt,
    }
]

try:
    while True:
        messages.append({"role": "user", "content": input("> ")})
        while True:
            response = client.chat.completions.create(
                model=model,
                tools=tools,
                messages=messages,
                extra_headers={"HTTP-Referer": "http://localhost", "X-Title": "Agent"},
            )
            msg = response.choices[0].message
            messages.append(msg.model_dump())
            if not msg.tool_calls:
                break
            checkpoint = len(messages) - 1
            cancelled = False
            for tc in msg.tool_calls:
                args = json.loads(tc.function.arguments)
                result, ok = run_tool(tc.function.name, args)
                if not ok:
                    cancelled = True
                    break
                messages.append(
                    {"role": "tool", "tool_call_id": tc.id, "content": str(result)}
                )
            if cancelled:
                del messages[checkpoint:]
                break
        print(msg.content)
except KeyboardInterrupt:
    print("BYEEE")
