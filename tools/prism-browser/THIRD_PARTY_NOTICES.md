# Third-party notices

## free-astra

`src/emulation.mjs` and parts of `src/protocol.mjs` are adapted from
`freeastra.py` of the free-astra project (https://github.com/Zhao73/free-astra):
the next-action-emitter prompt (`TOOL_PROTOCOL`, `TOOL_REMINDER`), the
environment and scaffolding patterns, Codex `additional_tools` handling,
transcript clamping, conversation flattening for Chat Completions and Responses,
tolerant parsing of tool calls and the `function_call` / `tool_calls` output
shapes. The code was ported from Python to JavaScript and changed.

free-astra is distributed under the MIT License:

```
MIT License

Copyright (c) 2026 free-astra contributors

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```
