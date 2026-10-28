# Docs

# Code style guidelines

- Replace em-dashes (—) with en-dashes (–) in text or comments
	- Use em-dashes only as an explicit empty placeholder value
- Replace en-dashes (–) with hyphens (-) in numerical or temporal ranges

## Go

- Use range-over-number `for i := range 5` instead of classic for loops `for i := 0; i < 5; i++` when the index is not mutated inside the loops
	- Same for range-over-slice `for i := range slice` instead of `for i := 0; i < len(slice); i++`
- Use guard clauses where possible to reduce nesting and improve readability
- Put function declarations above their usage in the same file, never below
- If multiple function parameters have the same type, group them like `func foo(a, b int)` instead of `func foo(a int, b int)`
- If multiple struct fields have the same type, group them like `type Foo struct { a, b int }` instead of `type Foo struct { a int; b int }`

## JS

- Use tabs for indentation
- Prefer double quotes over single quotes for strings
- Prefer string interpolation over concatenation
- Omit semicolons for separating statements
	- If required, for example where a statement starts with a parenthesis, see if it can be split up with a constant declaration
	- If a IIFE is needed, use `void` instead of a semicolon
- Blocks or conditionals with 1 statement should omit braces
	- This includes nested blocks, like `if (x) for (const y of z) ...`
- Arrow functions with only a single return expression should omit braces and the return keyword
- Function declarations should use arrow functions if they have only a single return expression, and standard function declarations otherwise
	- This also applies for top-level functions
	- Use `const` to declare arrow function variables
- Use guard statements to reduce nesting everywhere possible
	- Do this in functions with `return`, and in loops with `continue`
- Prefer `for (const x of y) ...` over `y.forEach(x => ...)`
- Prefer `const` over `let`
	- Avoid `var`
- Use `===` and `!==` over `==` and `!=` except when comparing with null
- Use Object.freeze() to make objects immutable where possible
- Use `void` before function calls that return promises to be discarded, or before IIFEs rather than semicolons
- Declare only 1 variable per declaration statement, avoid `const x = 1, y = 2, z = 3`
- When making large functions that return objects with many functions, consider if this would be more natural as a class with methods instead

None of this should conflict with the formatting or linting rules of Biome or oxlint.

## TS

- Use return types on functions
- Avoid `any` types
- Prefer union types over enums
- Prefer `interface` over `type` for object types
- Always use object types with indexers over Record types, for example prefer `{ [_: string]: string }` over `Record<string, string>`

## CSS

- Avoid `!important` where possible
	- Avoid the equivalent `!` suffix in Tailwind classes
- Avoid margins where possible
	- Replace with padding or gaps where spacing is needed
	- Replace with flex or grid for centreing
	- If needing negative margins for funky tricks, OK
- Use canonical Tailwind classes like `w-36` instead of `w-[9rem]`
- In the rare case where something is more concise in standard CSS than in Tailwind, write it in standard CSS

## Markdown

- Single * for italics, double ** for bold
- Use `#` for headings, not `=` or `-` underlines
- Use fenced code blocks with language identifiers instead of indented code blocks
- Do not hard-wrap. Never split a sentence or a paragraph over multiple lines

## Comments

- Do not hard-wrap. Never split a sentence over multiple lines
	- If the new line is after a semicolon or is a new sentence, OK
