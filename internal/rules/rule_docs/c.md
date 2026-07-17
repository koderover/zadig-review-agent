#### Obvious Typos or Spelling Errors
- Spelling errors in variable names, constant names, or function names at their declaration sites; do not report spelling errors at call sites
- Strings in log messages or exception messages containing spelling errors that affect readability

#### Proper malloc/free Pairing
**Key checks:**
- Every `malloc()` has a corresponding `free()`
- Avoid double-freeing the same memory block
- Set the pointer to NULL after freeing

**Example:**
```c
// Bad
char* buffer = malloc(1024);
// use buffer...
// forgot to free memory

// Good
char* buffer = malloc(1024);
if (buffer != NULL) {
    // use buffer...
    free(buffer);
    buffer = NULL;
}
```

#### Memory Leak Detection
**Key checks:**
- All allocated memory is freed before function exit
- Memory is also freed in error handling paths
- Use tools such as Valgrind for detection

#### Buffer Overflow Protection
**Key checks:**
- Check bounds before array access
- Use safe functions for string operations
- Ensure correct loop boundary conditions

**Example:**
```c
// Bad: dangerous operation
char buffer[100];
strcpy(buffer, user_input); // may overflow

// Good: safe operation
char buffer[100];
strncpy(buffer, user_input, sizeof(buffer) - 1);
buffer[sizeof(buffer) - 1] = '\0';
```

#### Safe String Operations
**Recommended safe functions:**
- Use size-aware string operations and verify bounds, truncation behavior, and null termination; replacing `strcpy()` with `strncpy()` alone is not sufficient
- Prefer `snprintf()` over unbounded formatting, and check its return value when truncation changes behavior

#### Naming Conventions
**Requirements:**
- Use snake_case naming style
- Variable names should be meaningful
- Constants should use UPPER_CASE
