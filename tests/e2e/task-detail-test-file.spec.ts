import { test, expect } from '@playwright/test';

// GET /api/tasks/{id} must name the test file `dicode task test` would run.
async function taskDetail(request: any, id: string) {
  const res = await request.get(`/api/tasks/${encodeURIComponent(id)}`);
  expect(res.status()).toBe(200);
  return res.json();
}

test.describe('Task detail test-file discovery', () => {
  test('python task reports task.test.py', async ({ request }) => {
    const detail = await taskDetail(request, 'e2e-tests/python-with-test');
    expect(detail.script_file).toBe('task.py');
    expect(detail.test_file).toBe('task.test.py');
    expect(detail.test_exists).toBe(true);
  });

  test('deno task without a test keeps the js default', async ({ request }) => {
    const detail = await taskDetail(request, 'e2e-tests/hello-manual');
    expect(detail.script_file).toBe('task.js');
    expect(detail.test_file).toBe('task.test.js');
    expect(detail.test_exists).toBe(false);
  });
});
