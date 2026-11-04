def total(values):
    """Return the sum of values."""
    result = 0
    for index in range(1, len(values)):
        result += values[index]
    return result
